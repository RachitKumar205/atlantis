package admin

import (
	"context"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Job rows belong to the caller that submitted them, and CAPABILITY_JOBS_READ
// is in the bundle every registered caller receives. Before owner_caller these
// RPCs selected by id alone, so any caller could read any other caller's job
// args and error text — in a product whose premise is that callers do not see
// each other's data.
//
// These drive the real SQL. The unit tests beside them cover which scope value
// a request resolves to; only a database can show that the predicate carrying
// it is written correctly, and a predicate that silently matches everything is
// indistinguishable from no predicate at all.

// jobScopeSvc builds a Service on an already-migrated pool, with a fixed
// identity and a fixed answer to "is this an operator".
func jobScopeSvc(t *testing.T, base *Service, caller string, operator bool) *Service {
	t.Helper()
	return New(base.pool, Config{
		AllowApplyMutation: true,
		CallerFromContext:  func(context.Context) string { return caller },
		HasCapability: func(_ context.Context, c adminpb.Capability) (bool, error) {
			return operator && c == adminpb.Capability_CAPABILITY_OPERATOR, nil
		},
	})
}

// seedJob inserts one live job owned by owner and returns its id as the string
// the RPCs take.
func seedJob(t *testing.T, svc *Service, owner string) string {
	t.Helper()
	var id int64
	err := svc.pool.QueryRow(context.Background(), `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, owner_caller)
VALUES ('ns.Work', 'default', '{}'::jsonb, 3, $1) RETURNING id`, owner).Scan(&id)
	if err != nil {
		t.Fatalf("seed job for %q: %v", owner, err)
	}
	return itoa(id)
}

func seedDeadJob(t *testing.T, svc *Service, owner string) string {
	t.Helper()
	var id int64
	err := svc.pool.QueryRow(context.Background(), `
INSERT INTO atlantis.jobs_dead
    (id, job_name, queue, args, attempts, max_retries, enqueued_at, owner_caller)
VALUES (nextval('atlantis.jobs_id_seq'), 'ns.Work', 'default', '{}'::jsonb, 3, 3, now(), $1)
RETURNING id`, owner).Scan(&id)
	if err != nil {
		t.Fatalf("seed dead job for %q: %v", owner, err)
	}
	return itoa(id)
}

func TestACallerCannotReadAnotherCallersJob(t *testing.T) {
	base := depScopeService(t)
	ctx := context.Background()

	mine := seedJob(t, base, "shop")
	theirs := seedJob(t, base, "billing")
	// owner_caller = '' is what every row submitted before migration 0028
	// carries. It must belong to nobody rather than to everybody.
	legacy := seedJob(t, base, "")

	shop := jobScopeSvc(t, base, "shop", false)
	for _, tc := range []struct {
		name string
		id   string
		want bool
	}{
		{"its own job", mine, true},
		{"another caller's job", theirs, false},
		{"a job with no recorded owner", legacy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := shop.GetJobStatus(ctx, &adminpb.GetJobStatusRequest{JobId: tc.id})
			if err != nil {
				t.Fatalf("GetJobStatus: %v", err)
			}
			if resp.GetFound() != tc.want {
				t.Errorf("Found = %v, want %v.\n"+
					"  A job the caller does not own must read as absent. Reporting it "+
					"as found — or refusing with a distinguishable error — answers the "+
					"question the scoping exists to refuse: whether that id is somebody's.",
					resp.GetFound(), tc.want)
			}
		})
	}

	// An operator reads across every caller, including the unattributed rows.
	// This is the half that keeps the console's queue view working, and it is
	// asserted so a later tightening cannot quietly empty that page.
	op := jobScopeSvc(t, base, "atlantis-console", true)
	for _, id := range []string{mine, theirs, legacy} {
		resp, err := op.GetJobStatus(ctx, &adminpb.GetJobStatusRequest{JobId: id})
		if err != nil {
			t.Fatalf("operator GetJobStatus(%s): %v", id, err)
		}
		if !resp.GetFound() {
			t.Errorf("operator cannot see job %s; the DLQ and queue views read through "+
				"this RPC and would show an empty queue", id)
		}
	}
}

func TestTheDeadLetterQueueIsScopedToItsOwner(t *testing.T) {
	base := depScopeService(t)
	ctx := context.Background()

	seedDeadJob(t, base, "shop")
	seedDeadJob(t, base, "billing")
	seedDeadJob(t, base, "")

	shop := jobScopeSvc(t, base, "shop", false)
	resp, err := shop.ListDeadJobs(ctx, &adminpb.ListDeadJobsRequest{})
	if err != nil {
		t.Fatalf("ListDeadJobs: %v", err)
	}
	if n := len(resp.GetJobs()); n != 1 {
		t.Errorf("shop sees %d dead jobs, want 1 — its own. The DLQ carries args and "+
			"error text, which is the most revealing thing in the table.", n)
	}

	op := jobScopeSvc(t, base, "atlantis-console", true)
	all, err := op.ListDeadJobs(ctx, &adminpb.ListDeadJobsRequest{})
	if err != nil {
		t.Fatalf("operator ListDeadJobs: %v", err)
	}
	if n := len(all.GetJobs()); n != 3 {
		t.Errorf("operator sees %d dead jobs, want 3; triage needs the whole queue", n)
	}
}

// TestRetryingAnotherCallersDeadJobDestroysNothing asserts that a refused
// retry is inert: it neither re-queues another caller's job nor removes it.
//
// What this actually pins is the scope on RetryDeadJob's INSERT ... SELECT.
// Removing it makes this test fail. It does NOT pin the matching predicate on
// the DELETE below — mutating that one to match every row leaves this test
// green, because RetryDeadJob returns on RowsAffected() == 0 before the DELETE
// runs. Said plainly because the obvious reading of the name is that both are
// covered, and a test believed to cover more than it does is worse than a
// missing one. jobs.go records why the redundant predicate stays.
func TestRetryingAnotherCallersDeadJobDestroysNothing(t *testing.T) {
	base := depScopeService(t)
	ctx := context.Background()
	theirs := seedDeadJob(t, base, "billing")

	shop := jobScopeSvc(t, base, "shop", false)
	if _, err := shop.RetryDeadJob(ctx, &adminpb.RetryDeadJobRequest{JobId: theirs}); err == nil {
		t.Fatal("shop retried billing's dead job")
	}

	var n int
	if err := base.pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.jobs_dead WHERE id = $1`, theirs).Scan(&n); err != nil {
		t.Fatalf("count dead: %v", err)
	}
	if n != 1 {
		t.Errorf("billing's dead job is gone after shop's refused retry (count = %d). "+
			"The refusal deleted the row it declined to move.", n)
	}
}

// visibleToSchema declares one job restricted to the "shop" caller and one
// open to everybody, so the same submit path can be driven across the branch.
const visibleToSchema = `
entity Ledger in shop {
  id bigint primary
}
job Restricted in shop {
  args { note text }
  visible_to "shop"
}
job Open in shop {
  args { note text }
  visible_to "*"
}
`

// TestVisibleToRefusesACallerTheJobDoesNotName covers the gate that used to
// read req.SubmittedBy.
//
// That value is filled by cmd/tide from $USER, so the check was decorative:
// any authenticated caller could type the name the job declared and submit.
// It now reads the mTLS identity, and the point of driving it through the real
// RPC is that a request CAN still carry a submitted_by claiming to be someone
// else — the test sends exactly that, and it must not help.
func TestVisibleToRefusesACallerTheJobDoesNotName(t *testing.T) {
	base := depScopeService(t)
	ctx := context.Background()
	depScopePlanAndApply(t, base, "shop", "jobs.atl", visibleToSchema)

	// billing lies in the request body, naming the caller the job allows.
	billing := jobScopeSvc(t, base, "billing", false)
	_, err := billing.SubmitJob(ctx, &adminpb.SubmitJobRequest{
		JobName:     "shop.Restricted",
		Args:        []byte(`{"note":"x"}`),
		SubmittedBy: "shop",
	})
	if err == nil {
		t.Error("billing submitted shop.Restricted by claiming SubmittedBy=shop.\n" +
			"  The gate is reading the request body again, which the client controls.")
	}

	// The caller the job names is allowed, so the gate refuses the right side.
	// Without this a gate that refused everybody would pass the test above.
	shop := jobScopeSvc(t, base, "shop", false)
	if _, err := shop.SubmitJob(ctx, &adminpb.SubmitJobRequest{
		JobName: "shop.Restricted", Args: []byte(`{"note":"x"}`),
	}); err != nil {
		t.Errorf("shop cannot submit its own job: %v", err)
	}

	// visible_to "*" restricts nobody.
	if _, err := billing.SubmitJob(ctx, &adminpb.SubmitJobRequest{
		JobName: "shop.Open", Args: []byte(`{"note":"x"}`),
	}); err != nil {
		t.Errorf(`billing refused a job declared visible_to "*": %v`, err)
	}
}

// TestSubmitJobRecordsTheVerifiedOwner closes the loop between the two halves:
// the identity the gate checks is the identity written to the row, so a job
// submitted by shop is one shop can later read back.
func TestSubmitJobRecordsTheVerifiedOwner(t *testing.T) {
	base := depScopeService(t)
	ctx := context.Background()
	depScopePlanAndApply(t, base, "shop", "jobs.atl", visibleToSchema)

	shop := jobScopeSvc(t, base, "shop", false)
	resp, err := shop.SubmitJob(ctx, &adminpb.SubmitJobRequest{
		JobName: "shop.Open", Args: []byte(`{"note":"x"}`), SubmittedBy: "cli:someone",
	})
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}

	var owner, submittedBy string
	if err := base.pool.QueryRow(ctx,
		`SELECT owner_caller, COALESCE(submitted_by, '') FROM atlantis.jobs WHERE id = $1`,
		resp.GetJobId()).Scan(&owner, &submittedBy); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if owner != "shop" {
		t.Errorf("owner_caller = %q, want %q — the row is unattributed, so the caller "+
			"that submitted it cannot read it back", owner, "shop")
	}
	// submitted_by keeps the client's claim, unverified and clearly separate.
	if submittedBy != "cli:someone" {
		t.Errorf("submitted_by = %q, want the client's own value; it is provenance for "+
			"a human and must not be quietly replaced by the identity", submittedBy)
	}

	got, err := shop.GetJobStatus(ctx, &adminpb.GetJobStatusRequest{JobId: resp.GetJobId()})
	if err != nil || !got.GetFound() {
		t.Errorf("shop cannot read back the job it just submitted (found=%v, err=%v)",
			got.GetFound(), err)
	}
}

// itoa avoids importing strconv for one call in one file.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
