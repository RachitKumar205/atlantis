package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The organisation boundary, exercised rather than asserted.
//
// A policy that isolates nothing passes every test that only ever looks at one
// organisation, so a fixture hardcoding one org cannot observe a boundary
// whether or not one exists. Every test here uses two organisations, and every
// scoped read is paired with a count taken as the superuser: a scoped read
// returning nothing and a broken policy returning nothing are the same
// observation, and only the ground-truth count separates them.
//
// Modelled on internal/runtime/partition_bind_pg_test.go and
// internal/server/entity/partition_endtoend_pg_test.go, which learned this the
// hard way on the caller side.

const (
	orgAcme   = "acme"
	orgGlobex = "globex"
)

// seedAudit writes one audit row per organisation through the real store, and
// returns the ground-truth total read as the superuser.
func seedAudit(t *testing.T, f *consoleFixture) int {
	t.Helper()
	ctx := context.Background()

	f.srv.db.forOrg(orgAcme).logAction(ctx,
		subjectFor(orgAcme, "a@example.com"), "a@example.com", "approve_plan", nil)
	f.srv.db.forOrg(orgAcme).logAction(ctx,
		subjectFor(orgAcme, "a@example.com"), "a@example.com", "rollback_schema", nil)
	f.srv.db.forOrg(orgGlobex).logAction(ctx,
		subjectFor(orgGlobex, "g@example.com"), "g@example.com", "approve_plan", nil)

	var total int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM console.audit_log`).Scan(&total); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	return total
}

// TestAuditRowsDoNotCrossOrganisations is the central case.
func TestAuditRowsDoNotCrossOrganisations(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	before := seedAudit(t, f)

	// Ground truth first. Without it, every assertion below is satisfied by a
	// policy that admits nothing at all — which is not isolation, it is an
	// outage that happens to look secure.
	if before < 3 {
		t.Fatalf("seeded %d audit rows, want at least 3; the reads below would be "+
			"unable to distinguish a working boundary from an empty table", before)
	}

	acme, err := f.srv.db.forOrg(orgAcme).listAuditLog(ctx, 100)
	if err != nil {
		t.Fatalf("list as acme: %v", err)
	}
	globex, err := f.srv.db.forOrg(orgGlobex).listAuditLog(ctx, 100)
	if err != nil {
		t.Fatalf("list as globex: %v", err)
	}

	if len(acme) == 0 || len(globex) == 0 {
		t.Fatalf("acme saw %d rows and globex %d; neither should be empty", len(acme), len(globex))
	}
	if len(acme)+len(globex) != before {
		t.Errorf("acme %d + globex %d != %d total: the organisations either overlap or something is missing",
			len(acme), len(globex), before)
	}
	for _, e := range acme {
		if !strings.Contains(e.Actor, orgAcme) {
			t.Errorf("acme saw an entry by %q", e.Actor)
		}
	}
	for _, e := range globex {
		if !strings.Contains(e.Actor, orgGlobex) {
			t.Errorf("globex saw an entry by %q", e.Actor)
		}
	}
}

// TestReachingForAnotherOrganisationByNameReturnsNothing is the case a
// forgotten predicate produces: the query names the other organisation
// explicitly, and the policy has to be what stops it.
func TestReachingForAnotherOrganisationByNameReturnsNothing(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedAudit(t, f)

	var globexAsSuperuser int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.audit_log WHERE org = $1`, orgGlobex).Scan(&globexAsSuperuser); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	if globexAsSuperuser == 0 {
		t.Fatal("no globex rows exist, so the assertion below proves nothing")
	}

	var reached int
	err := f.srv.db.forOrg(orgAcme).tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM console.audit_log WHERE org = $1`, orgGlobex).Scan(&reached)
	})
	if err != nil {
		t.Fatalf("bound query: %v", err)
	}
	if reached != 0 {
		t.Errorf("bound to %s, a query naming %s returned %d of the %d rows that exist",
			orgAcme, orgGlobex, reached, globexAsSuperuser)
	}
}

// TestUnboundReadsReturnNothing: not an error — zero rows.
//
// Zero is the correct outcome and the dangerous one. A request that forgot to
// bind shows an empty page rather than a failure, so this pins the behaviour
// that makes the forgetting safe rather than silent.
func TestUnboundReadsReturnNothing(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	total := seedAudit(t, f)

	// Straight on the pool, with no organisation bound.
	var n int
	if err := f.srv.db.pool.QueryRow(ctx, `SELECT count(*) FROM console.audit_log`).Scan(&n); err != nil {
		t.Fatalf("unbound read: %v", err)
	}
	if n != 0 {
		t.Errorf("an unbound read returned %d of %d rows", n, total)
	}
}

// TestBindDoesNotSurviveItsTransaction. The bind is transaction-local, which is
// what makes it safe on a pooled connection — a later request borrowing the
// same backend must not inherit it.
func TestBindDoesNotSurviveItsTransaction(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedAudit(t, f)

	if _, err := f.srv.db.forOrg(orgAcme).listAuditLog(ctx, 100); err != nil {
		t.Fatalf("bound read: %v", err)
	}

	var n int
	if err := f.srv.db.pool.QueryRow(ctx, `SELECT count(*) FROM console.audit_log`).Scan(&n); err != nil {
		t.Fatalf("read after the bound transaction: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows visible after the bound transaction ended; the bind leaked", n)
	}
}

// TestForgedCrossOrganisationWriteIsRefused covers the WITH CHECK half.
//
// USING alone would let one organisation plant a row in another's audit log:
// invisible to the writer afterwards, and present in the victim's history. A
// write leak rather than a read leak, and just as much a breach.
func TestForgedCrossOrganisationWriteIsRefused(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	err := f.srv.db.forOrg(orgAcme).tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO console.audit_log (actor, actor_email, action, org)
			VALUES ('forger', 'f@example.com', 'planted', $1)`, orgGlobex)
		return err
	})
	if err == nil {
		t.Fatal("bound to acme, an insert attributed to globex was accepted")
	}
	// Attributable, not merely refused. A test satisfied by any error passes
	// with the policy deleted and a typo in the column list.
	if !strings.Contains(err.Error(), "audit_log_org_isolation") {
		t.Errorf("refused, but not by the boundary policy: %v", err)
	}
}

// TestLegacyAuditRowsBelongToNobody. Rows written before migration 0004 have no
// recoverable organisation and are parked at ”. console.current_org() never
// returns ” — that is what the nullif in its body is for — so they are visible
// to no organisation while remaining in the table.
func TestLegacyAuditRowsBelongToNobody(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedAudit(t, f)

	// Written as the superuser: no bound organisation can produce org = ''.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO console.audit_log (actor, actor_email, action, org)
		VALUES ('local:4242', 'departed@example.com', 'approve_plan', '')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	var parked int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.audit_log WHERE org = ''`).Scan(&parked); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	if parked != 1 {
		t.Fatalf("expected 1 parked row, found %d", parked)
	}

	for _, org := range []string{orgAcme, orgGlobex} {
		entries, err := f.srv.db.forOrg(org).listAuditLog(ctx, 100)
		if err != nil {
			t.Fatalf("list as %s: %v", org, err)
		}
		for _, e := range entries {
			if e.Actor == "local:4242" {
				t.Errorf("%s can see an unattributed legacy row", org)
			}
		}
	}
}

// TestReadingAPartitionDirectlyReturnsNothing.
//
// A child partition inherits none of its parent's row-level security — not the
// switches, not the policies. Measured on PostgreSQL 17: before
// ensureAuditPartition began forcing them, reading a child directly returned
// every organisation's rows, bound or unbound, while the parent behaved
// correctly. The organisation boundary simply was not on the child.
//
// The fix is ENABLE + FORCE with no policy of its own, which is deny-all on
// direct access. Nothing reads children directly — the console queries the
// parent, and retention uses DROP TABLE — so a direct read is a bug or an
// attack, and both deserve nothing.
//
// Without this test, deleting the FORCE from ensureAuditPartition breaks
// nothing observable: every console query goes through the parent and keeps
// working.
func TestReadingAPartitionDirectlyReturnsNothing(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedAudit(t, f)

	// Find the partition holding the rows just written.
	//
	// relkind = 'r' matters: relispartition is true for partitioned indexes
	// too, so without it the query picks up audit_log_pNNNN_pkey and fails
	// with "cannot open relation".
	var partition string
	if err := f.pool.QueryRow(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'console' AND c.relispartition
		   AND c.relkind = 'r'
		   AND c.relname LIKE 'audit_log_p%'
		 ORDER BY c.relname LIMIT 1`).Scan(&partition); err != nil {
		t.Fatalf("find a partition: %v", err)
	}

	var inPartition int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.`+partition).Scan(&inPartition); err != nil {
		t.Fatalf("ground truth for %s: %v", partition, err)
	}
	if inPartition == 0 {
		t.Fatalf("%s is empty, so the assertions below would pass whatever the child's isolation was", partition)
	}

	// Bound to acme, reading the child directly.
	var boundDirect int
	if err := f.srv.db.forOrg(orgAcme).tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM console.`+partition).Scan(&boundDirect)
	}); err != nil {
		t.Fatalf("bound direct read: %v", err)
	}
	if boundDirect != 0 {
		t.Errorf("reading %s directly returned %d of %d rows; the partition is not isolated",
			partition, boundDirect, inPartition)
	}

	// And unbound, which is the shape an attacker or a stray query would take.
	var unboundDirect int
	if err := f.srv.db.pool.QueryRow(ctx,
		`SELECT count(*) FROM console.`+partition).Scan(&unboundDirect); err != nil {
		t.Fatalf("unbound direct read: %v", err)
	}
	if unboundDirect != 0 {
		t.Errorf("an unbound direct read of %s returned %d of %d rows",
			partition, unboundDirect, inPartition)
	}
}

// TestAnEmptyOrganisationCannotBind. The handle fails closed rather than
// binding nothing.
//
// Binding nothing would leave console.current_org() NULL, under which the
// RESTRICTIVE policy admits nothing — so the caller would see an empty audit
// log rather than an error. "The page is blank" is a much harder thing to
// diagnose than a refusal, and it is indistinguishable from an organisation
// that has genuinely done nothing yet.
func TestAnEmptyOrganisationCannotBind(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedAudit(t, f)

	_, err := f.srv.db.forOrg("").listAuditLog(ctx, 100)
	if err == nil {
		t.Fatal("an empty organisation bound successfully")
	}
	if !errors.Is(err, ErrNoOrg) {
		t.Errorf("err = %v, want ErrNoOrg — the refusal has to be attributable, "+
			"or this passes for any unrelated failure", err)
	}
}

// TestSignOutAllDoesNotCrossOrganisations.
//
// `DELETE FROM console.sessions` with no predicate was the shipped behaviour,
// and it was correct only while one console served one organisation. An admin
// pressing a button labelled as affecting their own organisation would have
// signed out every other one.
func TestSignOutAllDoesNotCrossOrganisations(t *testing.T) {
	f := newConsoleFixture(t)

	acmeToken := f.signInToOrg(t, orgAcme, "admin@acme.example.com", "admin")
	globexToken := f.signInToOrg(t, orgGlobex, "admin@globex.example.com", "admin")

	f.elevate(t, acmeToken)
	w := f.post(t, "/api/auth/sign-out-all", "", acmeToken)
	if w.Code != http.StatusOK {
		t.Fatalf("sign-out-all: status %d, body %s", w.Code, w.Body.String())
	}

	// acme's own session is gone, which is what the button promises.
	if code := f.meStatus(t, acmeToken); code != http.StatusUnauthorized {
		t.Errorf("acme's session survived its own sign-out-all: status %d", code)
	}
	// globex is untouched.
	if code := f.meStatus(t, globexToken); code != http.StatusOK {
		t.Errorf("globex was signed out by acme's sign-out-all: status %d", code)
	}
}

// meStatus reports what /api/auth/me answers for a session token.
func (f *consoleFixture) meStatus(t *testing.T, token string) int {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/auth/me", "", token))
	return w.Code
}

// TestTheAuditPageIsScopedToTheSignedInOrganisation is the same property at the
// HTTP boundary, because that is where a user meets it.
func TestTheAuditPageIsScopedToTheSignedInOrganisation(t *testing.T) {
	f := newConsoleFixture(t)

	acmeToken := f.signInToOrg(t, orgAcme, "admin@acme.example.com", "admin")
	globexToken := f.signInToOrg(t, orgGlobex, "admin@globex.example.com", "admin")

	read := func(token string) []string {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/audit?limit=100", "", token))
		if w.Code != http.StatusOK {
			t.Fatalf("audit listing: status %d, body %s", w.Code, w.Body.String())
		}
		var got struct {
			Entries []struct {
				Actor string `json:"actor"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, 0, len(got.Entries))
		for _, e := range got.Entries {
			out = append(out, e.Actor)
		}
		return out
	}

	acmeActors := read(acmeToken)
	globexActors := read(globexToken)

	// Both signed in, so both wrote a signed_in row: neither listing is empty,
	// and each must contain only its own.
	if len(acmeActors) == 0 || len(globexActors) == 0 {
		t.Fatalf("acme saw %d entries, globex %d; neither should be empty",
			len(acmeActors), len(globexActors))
	}
	for _, a := range acmeActors {
		if strings.Contains(a, orgGlobex) {
			t.Errorf("acme's audit page shows %q", a)
		}
	}
	for _, a := range globexActors {
		if strings.Contains(a, orgAcme) {
			t.Errorf("globex's audit page shows %q", a)
		}
	}
}

// Fleet facts do not cross organisations.
//
// The rows carry a customer's schema version, their dead-job backlog and their
// last admin-plane error, which is why console.org_facts is policed where
// console.orgs is not.
func TestFleetFactsDoNotCrossOrganisations(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	// Both organisations have to exist, since org_facts references the registry.
	if err := f.srv.db.rememberOrg(ctx, orgGlobex); err != nil {
		t.Fatalf("remember %s: %v", orgGlobex, err)
	}

	now := time.Now().UTC()
	acmeJobs, globexJobs := int32(3), int32(9)
	for org, n := range map[string]*int32{orgAcme: &acmeJobs, orgGlobex: &globexJobs} {
		if err := f.srv.db.forOrg(org).upsertOrgFacts(ctx, orgFacts{
			Org: org, CollectedAt: now, Reachable: true, MeasuredFacts: true, DeadJobs: n,
		}); err != nil {
			t.Fatalf("record facts for %s: %v", org, err)
		}
	}

	// Ground truth first. Without it a policy admitting nothing passes every
	// assertion below.
	var total int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM console.org_facts`).Scan(&total); err != nil {
		t.Fatalf("ground truth: %v", err)
	}
	if total != 2 {
		t.Fatalf("the superuser sees %d rows, want 2; the writes did not land", total)
	}

	got, ok, err := f.srv.db.forOrg(orgAcme).orgFacts(ctx)
	if err != nil {
		t.Fatalf("read acme's facts: %v", err)
	}
	if !ok {
		t.Fatal("acme reads no facts of its own")
	}
	if got.Org != orgAcme {
		t.Errorf("acme read %s's row", got.Org)
	}
	if got.DeadJobs == nil || *got.DeadJobs != acmeJobs {
		t.Errorf("dead_jobs = %v, want acme's %d — globex's row was read", got.DeadJobs, acmeJobs)
	}
}

// Facts cannot be written for another organisation.
//
// The SELECT side is above; this is the WITH CHECK half, which upsertOrgFacts
// cannot reach because it writes console.current_org().
func TestFleetFactsCannotBeWrittenForAnotherOrganisation(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	if err := f.srv.db.rememberOrg(ctx, orgGlobex); err != nil {
		t.Fatalf("remember %s: %v", orgGlobex, err)
	}

	err := f.srv.db.forOrg(orgAcme).tx(ctx, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `
			INSERT INTO console.org_facts (org, collected_at, reachable)
			VALUES ($1, NOW(), true)
		`, orgGlobex)
		return execErr
	})
	if err == nil {
		t.Error("acme wrote a facts row for globex; the WITH CHECK half of the " +
			"policy is not holding")
	}
}
