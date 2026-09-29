package admin

import (
	"context"
	"strings"
	"testing"
)

// The apply compares CHECK constraints and column types with the declaration
// after its own DDL, so what the migration changes is not drift and what it
// leaves diverged is.

// applyApproved plans and applies src, approving the plan when the policy
// asks for it.
func applyApproved(t *testing.T, svc *Service, caller, path, src string) {
	t.Helper()
	plan := depScopePlan(t, svc, caller, path, src)
	err := depScopeApply(t, svc, caller, path, src, plan)
	if err != nil && strings.Contains(err.Error(), "needs approval") {
		approveStoredPlan(t, svc, plan.GetPlanId())
		err = depScopeApply(t, svc, caller, path, src, plan)
	}
	if err != nil {
		t.Fatalf("apply %q: %v", src, err)
	}
}

func TestAnApplyMayChangeWhatItDeclares(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
	}{
		{"new entity with a field check", []string{
			`entity T in dra { id bigint primary  v int check "v > 0" }`}},
		{"new field with a check", []string{
			`entity T in dra { id bigint primary }`,
			`entity T in dra { id bigint primary  v int check "v > 0" }`}},
		{"check added to a field", []string{
			`entity T in dra { id bigint primary  v int }`,
			`entity T in dra { id bigint primary  v int check "v > 0" }`}},
		{"check changed", []string{
			`entity T in dra { id bigint primary  v int check "v > 0" }`,
			`entity T in dra { id bigint primary  v int check "v >= 0" }`}},
		{"varchar widened", []string{
			`entity T in dra { id bigint primary  v varchar(10) }`,
			`entity T in dra { id bigint primary  v varchar(20) }`}},
		// The enum does not exist until the DDL runs, so the table's CHECK
		// renders only after it.
		{"new enum field beside a check", []string{
			`entity T in dra { id bigint primary  v int check "v > 0" }`,
			"enum Mood in dra { a, b }\nentity T in dra { id bigint primary  v int check \"v > 0\"  mood Mood }"}},
		{"partial unique index removed", []string{
			`entity T in dra { id bigint primary  sku text  gone_at timestamptz  unique index partial by sku where gone_at is null }`,
			`entity T in dra { id bigint primary  sku text  gone_at timestamptz }`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := depScopeService(t)
			for _, src := range c.steps {
				// The plan's warnings describe the database, not this
				// plan's own changes.
				plan := depScopePlan(t, svc, "dra", "t.atl", src)
				if len(plan.GetCheckDrift()) != 0 || len(plan.GetColumnDrift()) != 0 {
					t.Errorf("plan warns of drift it creates: %v %v",
						plan.GetCheckDrift(), plan.GetColumnDrift())
				}
				applyApproved(t, svc, "dra", "t.atl", src)
			}
		})
	}
}

// Drift made outside atlantis is refused, and the refused apply's DDL rolls
// back with it.
func TestAnApplyRefusesDriftItLeavesBehind(t *testing.T) {
	cases := []struct {
		name, outOfBand, want string
	}{
		{"check", `
ALTER TABLE atlantis.drr_t DROP CONSTRAINT drr_t_v_check;
ALTER TABLE atlantis.drr_t ADD CONSTRAINT drr_t_v_check CHECK (v > 5)`,
			"CHECK constraints diverge"},
		{"column type", `ALTER TABLE atlantis.drr_t ALTER COLUMN code TYPE varchar(5)`,
			"column type(s) diverge"},
		{"undeclared check on a live column", `ALTER TABLE atlantis.drr_t ADD CONSTRAINT drr_t_extra CHECK (id > 0)`,
			`live constraint "drr_t_extra" is NOT declared`},
		{"undeclared unique index", `CREATE UNIQUE INDEX drr_t_code_uq ON atlantis.drr_t (code)`,
			"UNIQUE index"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := depScopeService(t)
			ctx := context.Background()
			applyApproved(t, svc, "drr", "t.atl",
				`entity T in drr { id bigint primary  v int check "v > 0"  code varchar(10) }`)
			if _, err := svc.pool.Exec(ctx, c.outOfBand); err != nil {
				t.Fatalf("out-of-band change: %v", err)
			}

			// The new enum field keeps the table from rendering before the
			// DDL, so every case is found by the render after it.
			next := "enum Mood in drr { a, b }\n" +
				`entity T in drr { id bigint primary  v int check "v > 0"  code varchar(10)  note text  mood Mood }`
			plan := depScopePlan(t, svc, "drr", "t.atl", next)
			err := depScopeApply(t, svc, "drr", "t.atl", next, plan)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("apply over out-of-band drift: %v; want %q", err, c.want)
			}
			var n int
			if err := svc.pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'drr_t' AND column_name = 'note'`).Scan(&n); err != nil {
				t.Fatalf("check the column: %v", err)
			}
			if n != 0 {
				t.Error("the refused apply left its column behind")
			}
		})
	}
}

// A parked column keeps its CHECK constraint, and later applies to its table
// go through.
func TestAParkedCheckedColumnBlocksNothing(t *testing.T) {
	svc := depScopeService(t)
	applyApproved(t, svc, "drp", "t.atl", `entity T in drp { id bigint primary  v int check "v > 0" }`)
	applyApproved(t, svc, "drp", "t.atl", `entity T in drp { id bigint primary }`)

	next := `entity T in drp { id bigint primary  note text }`
	plan := depScopePlan(t, svc, "drp", "t.atl", next)
	if len(plan.GetCheckDrift()) != 0 {
		t.Errorf("the plan reports the parked column's check as drift: %v", plan.GetCheckDrift())
	}
	if err := depScopeApply(t, svc, "drp", "t.atl", next, plan); err != nil {
		t.Fatalf("apply beside a parked checked column: %v", err)
	}
	var constraints int
	if err := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_constraint
 WHERE conrelid = 'atlantis.drp_t'::regclass AND contype = 'c'`).Scan(&constraints); err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	if constraints != 1 {
		t.Errorf("%d CHECK constraints on the table, want the parked column's 1", constraints)
	}
}

// A type change planned from the checkpoint is refused when the live column
// has a third type: numeric(12,4) altered to numeric(12,2) rounds every value.
// Declaring the live type is the way through.
func TestAnApplyRefusesATypeChangeFromAStaleType(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	applyApproved(t, svc, "drs", "t.atl", `entity T in drs { id bigint primary  amount numeric(10, 2) }`)
	if _, err := svc.pool.Exec(ctx, `
ALTER TABLE atlantis.drs_t ALTER COLUMN amount TYPE numeric(12, 4);
INSERT INTO atlantis.drs_t VALUES (1, 1.2345)`); err != nil {
		t.Fatalf("out-of-band change: %v", err)
	}

	// The new enum field does not resolve before the DDL; the retyped column
	// is still compared.
	next := "enum Mood in drs { a }\n" +
		`entity T in drs { id bigint primary  amount numeric(12, 2)  mood Mood }`
	plan := depScopePlan(t, svc, "drs", "t.atl", next)
	err := depScopeApply(t, svc, "drs", "t.atl", next, plan)
	if err == nil || !strings.Contains(err.Error(), "last applied NUMERIC(10, 2), live numeric(12,4)") {
		t.Fatalf("apply from a stale type: %v; want the stale-type refusal", err)
	}
	var amount string
	if err := svc.pool.QueryRow(ctx, `SELECT amount::text FROM atlantis.drs_t WHERE id = 1`).Scan(&amount); err != nil {
		t.Fatalf("read: %v", err)
	}
	if amount != "1.2345" {
		t.Errorf("amount = %s after the refusal, want 1.2345", amount)
	}

	applyApproved(t, svc, "drs", "t.atl", `entity T in drs { id bigint primary  amount numeric(12, 4) }`)
}

// A changed `table` name emits no SQL. Until the table is renamed, the apply
// is refused: its checkpoint would name a table that does not exist.
func TestAnApplyRefusesATableItCannotFind(t *testing.T) {
	svc := depScopeService(t)
	applyApproved(t, svc, "drt", "t.atl", "entity T in drt {\n  table \"drt_old\"\n  id bigint primary\n}")

	next := "entity T in drt {\n  table \"drt_new\"\n  id bigint primary\n}"
	plan := depScopePlan(t, svc, "drt", "t.atl", next)
	err := depScopeApply(t, svc, "drt", "t.atl", next, plan)
	if err != nil && strings.Contains(err.Error(), "needs approval") {
		approveStoredPlan(t, svc, plan.GetPlanId())
		err = depScopeApply(t, svc, "drt", "t.atl", next, plan)
	}
	// A bare `table` name lives in public (schema.EntitySchema).
	if err == nil || !strings.Contains(err.Error(), `"public"."drt_new"`) {
		t.Fatalf("apply with the rename not run: %v; want a refusal naming public.drt_new", err)
	}

	if _, err := svc.pool.Exec(context.Background(), `ALTER TABLE public.drt_old RENAME TO drt_new`); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := depScopeApply(t, svc, "drt", "t.atl", next, plan); err != nil {
		t.Fatalf("apply after the rename: %v", err)
	}
}

// A plan warns of drift on a table it declares for the first time when the
// table already exists, since its CREATE does nothing there.
func TestAPlanWarnsOfDriftOnATableItAdopts(t *testing.T) {
	svc := depScopeService(t)
	applyApproved(t, svc, "drn", "t.atl", `entity Other in drn { id bigint primary }`)
	if _, err := svc.pool.Exec(context.Background(),
		`CREATE TABLE atlantis.drn_t (id bigint PRIMARY KEY, v int CHECK (v > 5))`); err != nil {
		t.Fatalf("create the table out of band: %v", err)
	}
	plan := depScopePlan(t, svc, "drn", "t.atl", `
entity Other in drn { id bigint primary }
entity T in drn { id bigint primary  v int check "v > 0" }`)
	if len(plan.GetCheckDrift()) == 0 {
		t.Error("the plan reports no drift on a table whose live CHECK differs from its declaration")
	}
}
