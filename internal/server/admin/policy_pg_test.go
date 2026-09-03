package admin

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

func policyEntry(t *testing.T, resp []*adminpb.ChangePolicyEntry, class adminpb.PlanClass) *adminpb.ChangePolicyEntry {
	t.Helper()
	for _, e := range resp {
		if e.GetChangeClass() == class {
			return e
		}
	}
	t.Fatalf("%s missing from the policy response; every settable class is meant to "+
		"be reported whether or not it has a row", class)
	return nil
}

// The store, against a real Postgres, because the parts worth doubting are the
// SQL: an upsert that named the wrong conflict target would silently insert a
// duplicate on a table whose primary key is the class, and a partial write that
// touched rows it was not given would be invisible in Go.
func TestChangePolicyRoundTrip(t *testing.T) {
	svc := policyTestService(t)
	ctx := context.Background()

	before, err := svc.GetChangePolicy(ctx, &adminpb.GetChangePolicyRequest{})
	if err != nil {
		t.Fatalf("GetChangePolicy: %v", err)
	}
	if len(before.GetEntries()) != len(ChangeClasses()) {
		t.Fatalf("policy reports %d classes, want %d", len(before.GetEntries()), len(ChangeClasses()))
	}
	// The migration's seed, as it stands on this database.
	if e := policyEntry(t, before.GetEntries(), adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE); !e.GetRequireApproval() {
		t.Error("destructive is seeded without requiring approval")
	}
	if e := policyEntry(t, before.GetEntries(), adminpb.PlanClass_PLAN_CLASS_ADDITIVE); e.GetRequireApproval() {
		t.Error("additive is seeded requiring approval; every apply would stop for a human")
	}

	// Set one class. The response must describe the state after the write.
	after, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "policy-round-trip",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass:     adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED,
			RequireApproval: true,
			ApproverRole:    "developer",
		}},
	})
	if err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}
	got := policyEntry(t, after.GetEntries(), adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED)
	if !got.GetRequireApproval() || got.GetApproverRole() != "developer" {
		t.Errorf("after the write: require_approval=%v approver_role=%q, want true/developer",
			got.GetRequireApproval(), got.GetApproverRole())
	}
	if got.GetUpdatedBy() != "policy-round-trip" || got.GetUpdatedAt() == "" {
		t.Errorf("audit columns not recorded: updated_by=%q updated_at=%q",
			got.GetUpdatedBy(), got.GetUpdatedAt())
	}

	// And it must have left every other class exactly as it found it. A full
	// replacement here is the bug the partial write exists to avoid: two
	// operators with the settings page open would each send the whole policy,
	// and the second save would revert the first one's unrelated edit.
	for _, class := range []adminpb.PlanClass{
		adminpb.PlanClass_PLAN_CLASS_ADDITIVE,
		adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING,
		adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
	} {
		was := policyEntry(t, before.GetEntries(), class)
		now := policyEntry(t, after.GetEntries(), class)
		if was.GetRequireApproval() != now.GetRequireApproval() ||
			was.GetApproverRole() != now.GetApproverRole() {
			t.Errorf("writing BACKFILL_REQUIRED also changed %s (%v/%q -> %v/%q)",
				class, was.GetRequireApproval(), was.GetApproverRole(),
				now.GetRequireApproval(), now.GetApproverRole())
		}
	}

	// Writing the same class twice updates rather than duplicating. change_class
	// is the primary key, so a wrong conflict target errors rather than
	// inserting — which is the good outcome, and worth having a test that would
	// see it.
	if _, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "policy-round-trip-2",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass: adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED, RequireApproval: false,
		}},
	}); err != nil {
		t.Fatalf("second SetChangePolicy: %v", err)
	}
	var rows int
	if err := svc.pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.change_policy WHERE change_class = 'PLAN_CLASS_BACKFILL_REQUIRED'`,
	).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Errorf("%d rows for BACKFILL_REQUIRED after two writes, want 1", rows)
	}
	// An omitted approver_role must fall back rather than be stored empty.
	final, err := svc.GetChangePolicy(ctx, &adminpb.GetChangePolicyRequest{})
	if err != nil {
		t.Fatalf("GetChangePolicy: %v", err)
	}
	if role := policyEntry(t, final.GetEntries(), adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED).GetApproverRole(); role != DefaultApproverRole {
		t.Errorf("approver_role after an omitted one = %q, want %q", role, DefaultApproverRole)
	}
}

// A row an operator deleted, and a row an operator mistyped, must both read
// back as requiring approval.
//
// The Go side carries this because the table has no CHECK on the class name.
// Asserted against the real table, so a CHECK added later turns the mistyped
// case into an INSERT failure and this notices the rule moved.
func TestADeletedOrMistypedRuleReadsAsRequiringApproval(t *testing.T) {
	svc := policyTestService(t)
	ctx := context.Background()

	if _, err := svc.pool.Exec(ctx,
		`DELETE FROM atlantis.change_policy WHERE change_class = 'PLAN_CLASS_ADDITIVE'`); err != nil {
		t.Fatalf("delete the additive rule: %v", err)
	}
	if _, err := svc.pool.Exec(ctx, `
INSERT INTO atlantis.change_policy (change_class, require_approval, updated_by)
VALUES ('PLAN_CLASS_ADITIVE', false, 'a-typo')`); err != nil {
		t.Fatalf("insert the mistyped rule: %v", err)
	}

	resp, err := svc.GetChangePolicy(ctx, &adminpb.GetChangePolicyRequest{})
	if err != nil {
		t.Fatalf("GetChangePolicy: %v", err)
	}
	if e := policyEntry(t, resp.GetEntries(), adminpb.PlanClass_PLAN_CLASS_ADDITIVE); !e.GetRequireApproval() {
		t.Error("additive read back as not requiring approval, with its row deleted and " +
			"a misspelling of its name in the table. A typo must not be able to " +
			"stand in for the rule it was meant to write.")
	}
	// The mistyped row must not appear in the response either — reporting a
	// class nobody can produce a diff for would invite an operator to configure it.
	for _, e := range resp.GetEntries() {
		if strings.Contains(e.GetChangeClass().String(), "ADITIVE") {
			t.Errorf("the mistyped class %s was reported as configurable", e.GetChangeClass())
		}
	}
}

// SetChangePolicy must refuse a class no rule can apply to, rather than storing
// a row that nothing will ever read.
func TestUnparseableIsNotSettable(t *testing.T) {
	svc := policyTestService(t)

	_, err := svc.SetChangePolicy(context.Background(), &adminpb.SetChangePolicyRequest{
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass: adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE, RequireApproval: false,
		}},
	})
	if err == nil {
		t.Fatal("a rule was accepted for PLAN_CLASS_UNPARSEABLE, which means the DSL " +
			"did not compile — there is no apply for it to govern")
	}
	if !strings.Contains(err.Error(), "not a class a rule can apply to") {
		t.Errorf("refused, but for some other reason: %v", err)
	}
}

// policyTestService opens a service against the shared test database and puts
// atlantis.change_policy back the way it found it.
//
// Restoring rather than using a private database, because these tests touch
// exactly one table: a private database would mean running the whole migration
// tree per test for a table the shared one already has.
//
// The snapshot is held in Go rather than in a TEMP table. A temp table belongs
// to one session, and a pool hands out a different connection whenever it feels
// like it — so the restore would run on a backend where the backup did not
// exist, fail, and leave the developer's database holding whatever the last
// test wrote.
func policyTestService(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the change-policy store")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	type snapshotRow struct {
		class, role, by string
		require         bool
		at              time.Time
	}
	var snapshot []snapshotRow
	rows, err := pool.Query(ctx, `
SELECT change_class, require_approval, approver_role, updated_at, updated_by
FROM atlantis.change_policy`)
	if err != nil {
		t.Fatalf("snapshot change_policy: %v", err)
	}
	for rows.Next() {
		var r snapshotRow
		if err := rows.Scan(&r.class, &r.require, &r.role, &r.at, &r.by); err != nil {
			rows.Close()
			t.Fatalf("scan snapshot: %v", err)
		}
		snapshot = append(snapshot, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot change_policy: %v", err)
	}
	if len(snapshot) == 0 {
		t.Fatal("atlantis.change_policy is empty on the test database — migration 0026 " +
			"has not run, and these tests would prove nothing about the seeded state")
	}

	t.Cleanup(func() {
		bg := context.Background()
		if _, err := pool.Exec(bg, `DELETE FROM atlantis.change_policy`); err != nil {
			t.Errorf("clear change_policy: %v", err)
		}
		for _, r := range snapshot {
			if _, err := pool.Exec(bg, `
INSERT INTO atlantis.change_policy (change_class, require_approval, approver_role, updated_at, updated_by)
VALUES ($1, $2, $3, $4, $5)`, r.class, r.require, r.role, r.at, r.by); err != nil {
				t.Errorf("restore %s: %v", r.class, err)
			}
		}
	})
	return New(pool, Config{AllowApplyMutation: true})
}

// The plan reports the rule, and the rule it reports is the one in the table.
//
// This is the wiring, not the store: a plan that always said "approval
// required" for destructive changes would pass a test that only ever looked at
// the seeded state, and would still be reporting a constant. So the policy is
// flipped underneath and the same plan is asked again.
func TestPlanReportsWhatTheChangePolicySays(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()

	const withNote = `
entity Ledger in dspol {
  id   bigint primary
  note text
}
`
	const withoutNote = `
entity Ledger in dspol {
  id bigint primary
}
`
	depScopePlanAndApply(t, svc, "dspol", "ledger.atl", withNote)

	// Dropping a column is destructive, which a fresh install seeds as
	// requiring approval.
	plan := depScopePlan(t, svc, "dspol", "ledger.atl", withoutNote)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE {
		t.Fatalf("dropping a column planned as %s; this test is not exercising what "+
			"it thinks it is", plan.GetClass())
	}
	if !plan.GetRequiresApproval() {
		t.Error("a destructive plan reported requires_approval=false against a policy " +
			"that requires it")
	}
	if plan.GetApproverRole() != DefaultApproverRole {
		t.Errorf("approver_role = %q, want %q", plan.GetApproverRole(), DefaultApproverRole)
	}

	// Now say destructive changes may apply unattended, and ask again.
	if _, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "plan-reporting-test",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass: adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE, RequireApproval: false,
		}},
	}); err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}
	relaxed := depScopePlan(t, svc, "dspol", "ledger.atl", withoutNote)
	if relaxed.GetRequiresApproval() {
		t.Error("the policy was set to not require approval for destructive changes " +
			"and the plan still reports that it does — the field is not reading the table")
	}
	if relaxed.GetApproverRole() != "" {
		t.Errorf("approver_role = %q with no approval required; naming a role there "+
			"implies a gate that is not present", relaxed.GetApproverRole())
	}
}

// The seeding SQL, run for real, on both sides of the branch it contains.
//
// This is the highest-risk part of 0026 and the part no Go code touches. The
// backfill_required rule is seeded by install age: a fresh install gets the
// safe default, an upgrade keeps working the way its CI already does. Getting
// that backwards means either a new deployment applying backfills unattended,
// or every existing customer's pipeline stopping on the release that adds a
// feature they did not ask for.
//
// The migrate is version-stepped. Running the tree to the end and then
// inspecting rows exercises only the fresh-install branch, because the other
// needs schema history to exist before 0026 runs.
func TestTheMigrationSeedsByInstallAge(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the change-policy seeding")
	}

	const beforePolicy = 25 // the migration immediately before 0026

	cases := []struct {
		name string
		// priorHistory writes whatever must exist before 0026 runs.
		priorHistory string
		wantBackfill bool
	}{{
		name:         "a fresh install has no history to break",
		priorHistory: "",
		wantBackfill: true,
	}, {
		name: "an upgrade keeps applying backfills the way it already does",
		// An 'apply' event is what distinguishes a deployment somebody has
		// used from one 0012 merely seeded a checkpoint row into.
		priorHistory: `
INSERT INTO atlantis.schema_versions (caller, plan_class, diff, ir_snapshot, ir_hash, event_type)
VALUES ('someteam', 'additive', '{}'::jsonb, '{}'::jsonb, 'deadbeef', 'apply')`,
		wantBackfill: false,
	}, {
		name: "a checkpoint seeded by 0012 is not history anybody made",
		priorHistory: `
INSERT INTO atlantis.schema_versions (caller, plan_class, diff, ir_snapshot, ir_hash, event_type)
VALUES ('system', 'seed', '{}'::jsonb, '{}'::jsonb, 'cafe', 'seed')`,
		wantBackfill: true,
	}}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := freshMigrationDatabase(t, adminDSN, fmt.Sprintf("atlantis_seed_%d", i))
			m := openInfraMigrate(t, dsn)

			if err := m.Migrate(beforePolicy); err != nil {
				t.Fatalf("migrate to %d: %v", beforePolicy, err)
			}
			pool, err := pgxpool.New(context.Background(), dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer pool.Close()
			if tc.priorHistory != "" {
				if _, err := pool.Exec(context.Background(), tc.priorHistory); err != nil {
					t.Fatalf("write prior history: %v", err)
				}
			}

			if err := m.Migrate(beforePolicy + 1); err != nil {
				t.Fatalf("migrate to %d: %v", beforePolicy+1, err)
			}

			var got bool
			if err := pool.QueryRow(context.Background(), `
SELECT require_approval FROM atlantis.change_policy
WHERE change_class = 'PLAN_CLASS_BACKFILL_REQUIRED'`).Scan(&got); err != nil {
				t.Fatalf("read the seeded rule: %v", err)
			}
			if got != tc.wantBackfill {
				t.Errorf("backfill_required seeded as require_approval=%v, want %v", got, tc.wantBackfill)
			}

			// The two that are not install-age dependent, checked here too so a
			// future edit cannot make one of them conditional unnoticed.
			for class, want := range map[string]bool{
				"PLAN_CLASS_ADDITIVE":              false,
				"PLAN_CLASS_CROSS_CALLER_BREAKING": true,
				"PLAN_CLASS_DESTRUCTIVE":           true,
			} {
				var seeded bool
				if err := pool.QueryRow(context.Background(),
					`SELECT require_approval FROM atlantis.change_policy WHERE change_class = $1`,
					class).Scan(&seeded); err != nil {
					t.Fatalf("read %s: %v", class, err)
				}
				if seeded != want {
					t.Errorf("%s seeded as %v, want %v regardless of install age", class, seeded, want)
				}
			}
		})
	}
}

// No identity may hold CAPABILITY_SCHEMA_APPLY and CAPABILITY_SCHEMA_APPROVE
// at once — an identity holding both is a caller that approves its own
// changes. capability.proto states the rule; this holds the migration tree to
// it, so a future grant reintroducing the pair fails here rather than in an
// audit.
//
// Runs the whole tree: the pair existed between 0034 and 0038, and a
// version-stepped check pinned to today's numbering would miss a later grant.
func TestNoIdentityHoldsBothApplyAndApprove(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the capability separation")
	}
	dsn := freshMigrationDatabase(t, adminDSN, "atlantis_cap_separation")
	m := openInfraMigrate(t, dsn)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	rows, err := pool.Query(context.Background(), `
		SELECT caller FROM atlantis.caller_capabilities
		WHERE capability = 'CAPABILITY_SCHEMA_APPLY'
		INTERSECT
		SELECT caller FROM atlantis.caller_capabilities
		WHERE capability = 'CAPABILITY_SCHEMA_APPROVE'`)
	if err != nil {
		t.Fatalf("query the intersection: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var caller string
		if err := rows.Scan(&caller); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("%q holds both CAPABILITY_SCHEMA_APPLY and CAPABILITY_SCHEMA_APPROVE", caller)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// freshMigrationDatabase creates an empty database with the extensions the
// migration tree needs, and drops it afterwards.
func freshMigrationDatabase(t *testing.T, adminDSN, name string) string {
	t.Helper()
	return pgcatalog.PrivateDatabase(t, adminDSN, name)
}

// openInfraMigrate drives the infra migration tree by version, which
// internal/migrate does not expose: the server runs everything, and a partial
// migration is a test-only need.
func openInfraMigrate(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	url := strings.TrimPrefix(strings.TrimPrefix(dsn, "postgres://"), "postgresql://")
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	// search_path pinned for the same reason internal/migrate pins it: without
	// it golang-migrate's unqualified version table moves between `public` and
	// `atlantis` depending on whether the schema exists yet, and the version it
	// reads back is not the version it wrote.
	m, err := migrate.New("file://../../../migrations/infra",
		"pgx5://"+url+sep+"search_path=public&x-migrations-table=atlantis_schema_migrations_infra")
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}
