package admin

import (
	"context"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Rehearsal end to end: clone the managed database, execute the real up_sql
// there, and read the verdict off what Postgres did. These run on the
// depScope private database, whose role owns it and may CREATE DATABASE, so
// the same-cluster fallback target resolves.

const (
	rehearseV1 = `
entity Reading in %s {
  id    bigint primary
  email text
}
`
	// Making email NOT NULL without a default or backfill: additive-plus
	// enough to emit SET NOT NULL, which the seeded NULL row defeats.
	rehearseV2NotNull = `
entity Reading in %s {
  id    bigint primary
  email text not null backfill "'unknown'"
}
`
	rehearseV2Additive = `
entity Reading in %s {
  id    bigint primary
  email text
  note  text
}
`
)

func rehearseAdminFixture(t *testing.T, svc *Service, ns string) {
	t.Helper()
	depScopePlanAndApply(t, svc, ns, "reading.atl", tierSrc(rehearseV1, ns))
	if _, err := svc.RegisterCaller(context.Background(), &adminpb.RegisterCallerRequest{
		Caller: ns, CanMutate: true, CreatedBy: "rehearse-test",
	}); err != nil {
		t.Fatalf("RegisterCaller: %v", err)
	}
	// A row a NOT NULL would refuse, and rows a pass copies.
	if _, err := svc.pool.Exec(context.Background(), `
INSERT INTO atlantis.`+ns+`_reading (id, email) VALUES (1, 'a@example.com'), (2, NULL)`); err != nil {
		t.Fatalf("seed managed rows: %v", err)
	}
}

func rehearseFiles(ns, src string) []*adminpb.SubmittedFile {
	return []*adminpb.SubmittedFile{{Path: "reading.atl", Content: []byte(tierSrc(src, ns))}}
}

func TestRehearsalReadsTheVerdictOffTheClone(t *testing.T) {
	svc := depScopeService(t)
	rehearseAdminFixture(t, svc, "rhverdict")
	ctx := context.Background()

	// The NOT NULL change fails on the seeded NULL: fail_data, with the
	// constraint named, a count, and a remediation.
	resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: "rhverdict", Files: rehearseFiles("rhverdict", rehearseV2NotNull),
	})
	if err != nil {
		t.Fatalf("RehearseMigration: %v", err)
	}
	if resp.GetVerdict() != "fail_data" {
		t.Fatalf("verdict = %q (%s / %s), want fail_data",
			resp.GetVerdict(), resp.GetReason(), resp.GetError())
	}
	if resp.GetSqlstate() != "23502" {
		t.Errorf("sqlstate = %q, want 23502", resp.GetSqlstate())
	}
	if strings.Contains(resp.GetError(), "a@example.com") {
		t.Errorf("the redacted error carries a row value: %s", resp.GetError())
	}
	if !strings.Contains(resp.GetRemediation(), "1 rows hold NULL") &&
		!strings.Contains(resp.GetRemediation(), "backfill") {
		t.Errorf("remediation says nothing usable: %q", resp.GetRemediation())
	}

	// The additive change passes.
	resp, err = svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: "rhverdict", Files: rehearseFiles("rhverdict", rehearseV2Additive),
	})
	if err != nil {
		t.Fatalf("RehearseMigration(additive): %v", err)
	}
	if resp.GetVerdict() != "pass" {
		t.Fatalf("verdict = %q (%s / %s), want pass",
			resp.GetVerdict(), resp.GetReason(), resp.GetError())
	}

	// Nothing wearing the clone prefix survives the RPC.
	var leaked int
	if err := svc.pool.QueryRow(ctx, `
SELECT count(*) FROM pg_database WHERE datname LIKE 'atlantis_rehearsal_%'`).Scan(&leaked); err != nil {
		t.Fatalf("count clones: %v", err)
	}
	if leaked != 0 {
		t.Errorf("%d clone databases leaked", leaked)
	}
	// And the registry says so.
	var creating int
	if err := svc.pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.rehearsal_clones WHERE state = 'creating'`).Scan(&creating); err != nil {
		t.Fatalf("count registry: %v", err)
	}
	if creating != 0 {
		t.Errorf("%d registry rows still creating", creating)
	}
}

func TestListRedactsWhatGetReturns(t *testing.T) {
	svc := depScopeService(t)
	rehearseAdminFixture(t, svc, "rhredact")
	ctx := context.Background()

	// A duplicate, so a new UNIQUE fails with a DETAIL line that embeds the
	// row value — the exact surface the redaction exists for.
	if _, err := svc.pool.Exec(ctx, `
INSERT INTO atlantis.rhredact_reading (id, email) VALUES (3, 'a@example.com')`); err != nil {
		t.Fatalf("seed duplicate: %v", err)
	}
	resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: "rhredact", Files: rehearseFiles("rhredact", `
entity Reading in %s {
  id    bigint primary
  email text
  unique by email
}
`),
	})
	if err != nil {
		t.Fatalf("RehearseMigration: %v", err)
	}
	if resp.GetVerdict() != "fail_data" || resp.GetSqlstate() != "23505" {
		t.Fatalf("verdict = %q sqlstate %q (%s / %s), want fail_data 23505",
			resp.GetVerdict(), resp.GetSqlstate(), resp.GetReason(), resp.GetError())
	}
	if strings.Contains(resp.GetError(), "a@example.com") {
		t.Fatalf("the redacted error carries the row value: %s", resp.GetError())
	}

	list, err := svc.ListRehearsals(ctx, &adminpb.ListRehearsalsRequest{Caller: "rhredact"})
	if err != nil {
		t.Fatalf("ListRehearsals: %v", err)
	}
	if len(list.GetRehearsals()) != 1 {
		t.Fatalf("%d rehearsals listed, want 1", len(list.GetRehearsals()))
	}
	// The summary type carries no detail field; what it does carry must not
	// smuggle one in.
	if s := list.GetRehearsals()[0]; strings.Contains(s.GetError(), "Failing row") ||
		strings.Contains(s.GetError(), "a@example.com") {
		t.Errorf("the listed error carries detail text: %s", s.GetError())
	}

	got, err := svc.GetRehearsal(ctx, &adminpb.GetRehearsalRequest{RehearsalId: resp.GetRehearsalId()})
	if err != nil {
		t.Fatalf("GetRehearsal: %v", err)
	}
	if got.GetErrorDetail() == "" {
		t.Error("the full record carries no detail; the rehearse capability is what gates it")
	}
}

// TestRehearsalCountsTheDuplicates covers a unique field and a unique partial
// index. The error names only the index, so the columns come from the
// declaration. NULL keys and rows outside the predicate are exempt.
func TestRehearsalCountsTheDuplicates(t *testing.T) {
	svc := depScopeService(t)
	rehearseAdminFixture(t, svc, "rhdup")
	ctx := context.Background()

	// With the fixture's (1, a@) and (2, NULL): a@ three times, b@ twice.
	if _, err := svc.pool.Exec(ctx, `
INSERT INTO atlantis.rhdup_reading (id, email) VALUES
    (3, 'a@example.com'), (4, 'a@example.com'), (5, 'b@example.com'),
    (6, 'b@example.com'), (7, 'c@example.com'), (8, NULL)`); err != nil {
		t.Fatalf("seed duplicates: %v", err)
	}

	for _, tc := range []struct {
		name, src, key, counts string
		rows                   int64
	}{
		{
			name: "unique field",
			src: `
entity Reading in %s {
  id    bigint primary
  email text unique
}
`,
			key:    "rhdup_reading.email duplicate",
			counts: "5 rows in rhdup_reading share 2 values of email",
			rows:   5,
		},
		{
			// id 5 is outside the predicate, so b@ has one row inside it. The
			// `unique by` builds, so the index that fails is the second rule.
			name: "unique partial index",
			src: `
entity Reading in %s {
  id    bigint primary
  email text
  unique by id, email
  unique index partial by email where id != 5
}
`,
			key:    "rhdup_reading.email duplicate",
			counts: "3 rows in rhdup_reading share 1 value of email",
			rows:   3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
				Caller: "rhdup", Files: rehearseFiles("rhdup", tc.src),
			})
			if err != nil {
				t.Fatalf("RehearseMigration: %v", err)
			}
			if resp.GetVerdict() != "fail_data" || resp.GetSqlstate() != "23505" {
				t.Fatalf("verdict = %q sqlstate %q (%s / %s), want fail_data 23505",
					resp.GetVerdict(), resp.GetSqlstate(), resp.GetReason(), resp.GetError())
			}
			if !strings.Contains(resp.GetRemediation(), tc.counts) {
				t.Errorf("remediation = %q, want it to say %q", resp.GetRemediation(), tc.counts)
			}
			if strings.Contains(resp.GetRemediation(), "@example.com") {
				t.Errorf("the remediation carries a row value: %s", resp.GetRemediation())
			}
			if got := resp.GetDiagnostics(); len(got) != 1 || got[tc.key] != tc.rows {
				t.Errorf("diagnostics = %v, want %s: %d", got, tc.key, tc.rows)
			}
		})
	}
}

// The whole point: a verified destructive change applies unattended at
// auto_all, and only a fresh matching pass does it.
func TestAutoAllConsumesAFreshPass(t *testing.T) {
	svc := depScopeService(t)
	rehearseAdminFixture(t, svc, "rhauto")
	ctx := context.Background()

	setTier(t, svc, "rhauto", "auto_all")
	if _, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "rehearse-test",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass:     adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
			RequireApproval: false,
		}},
	}); err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}

	const dropEmail = `
entity Reading in %s {
  id bigint primary
}
`
	// Unrehearsed: refused — unverified never auto-applies.
	plan := depScopePlan(t, svc, "rhauto", "reading.atl", tierSrc(dropEmail, "rhauto"))
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE {
		t.Fatalf("the change planned as %s, not destructive", plan.GetClass())
	}
	err := depScopeApply(t, svc, "rhauto", "reading.atl", tierSrc(dropEmail, "rhauto"), plan)
	if err == nil {
		t.Fatal("an unverified destructive change ran unattended at auto_all")
	}

	resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: "rhauto", Files: rehearseFiles("rhauto", dropEmail),
	})
	if err != nil {
		t.Fatalf("RehearseMigration: %v", err)
	}
	if resp.GetVerdict() != "pass" {
		t.Fatalf("verdict = %q (%s / %s), want pass — a parked drop runs clean",
			resp.GetVerdict(), resp.GetReason(), resp.GetError())
	}

	// Now it applies with no human, and the version row says what
	// authorized it.
	if err := depScopeApply(t, svc, "rhauto", "reading.atl", tierSrc(dropEmail, "rhauto"), plan); err != nil {
		t.Fatalf("a verified destructive change was still refused: %v", err)
	}
	var policy, verdict string
	if err := svc.pool.QueryRow(ctx, `
SELECT applied_under_policy, applied_verdict FROM atlantis.schema_versions
WHERE caller = 'rhauto' ORDER BY version DESC LIMIT 1`).Scan(&policy, &verdict); err != nil {
		t.Fatalf("read version row: %v", err)
	}
	if policy != "auto_all" || verdict != "pass" {
		t.Errorf("version row says (%q, %q), want (auto_all, pass)", policy, verdict)
	}

	// And the rehearsal's promise is reconciled.
	var outcome string
	if err := svc.pool.QueryRow(ctx, `
SELECT outcome FROM atlantis.rehearsals WHERE rehearsal_id = $1`,
		resp.GetRehearsalId()).Scan(&outcome); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if outcome != "applied" {
		t.Errorf("outcome = %q, want applied", outcome)
	}
}

// The plan-id mode: the console rehearses a queued request without holding
// the caller's files.
func TestRehearsalByPlanIDStampsThePlanRow(t *testing.T) {
	svc := depScopeService(t)
	rehearseAdminFixture(t, svc, "rhplan")
	ctx := context.Background()

	// A destructive change queues under the default policy.
	plan := depScopePlan(t, svc, "rhplan", "reading.atl", tierSrc(`
entity Reading in %s {
  id bigint primary
}
`, "rhplan"))
	if err := depScopeApply(t, svc, "rhplan", "reading.atl", tierSrc(`
entity Reading in %s {
  id bigint primary
}
`, "rhplan"), plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}

	resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		PlanId: plan.GetPlanId(),
	})
	if err != nil {
		t.Fatalf("RehearseMigration(plan_id): %v", err)
	}
	if resp.GetVerdict() != "pass" {
		t.Fatalf("verdict = %q (%s / %s), want pass — a parked drop runs clean",
			resp.GetVerdict(), resp.GetReason(), resp.GetError())
	}

	list, err := svc.ListSchemaPlans(ctx, &adminpb.ListSchemaPlansRequest{Caller: "rhplan"})
	if err != nil {
		t.Fatalf("ListSchemaPlans: %v", err)
	}
	if len(list.GetPlans()) == 0 || list.GetPlans()[0].GetRehearsalVerdict() != "pass" {
		t.Errorf("the plan row does not carry the verdict: %+v", list.GetPlans())
	}
}
