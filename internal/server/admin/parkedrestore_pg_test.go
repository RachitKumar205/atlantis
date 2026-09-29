package admin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Re-declaring a dropped field, through PlanSchema and ApplyMigration against
// a live database. The classification depends on what the database holds, so
// only a database can show it is right.

const (
	restoreV1 = `
entity Invoice in rst {
  id            bigint primary
  customer_name text not null
  code          varchar(10)
}
`
	restoreDropped = `
entity Invoice in rst {
  id bigint primary
}
`
)

// restoreParked applies restoreV1, writes 300 rows, and parks customer_name
// and code through an approved destructive plan.
func restoreParked(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	depScopePlanAndApply(t, svc, "rst", "invoice.atl", restoreV1)
	if _, err := svc.pool.Exec(ctx, `
INSERT INTO atlantis.rst_invoice
SELECT g, 'customer ' || g, 'C' || g FROM generate_series(1, 300) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	plan := depScopePlan(t, svc, "rst", "invoice.atl", restoreDropped)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE {
		t.Fatalf("dropping two columns planned as %s, want destructive", plan.GetClass())
	}
	if err := depScopeApply(t, svc, "rst", "invoice.atl", restoreDropped, plan); err == nil {
		t.Fatal("the drop applied without approval; the fixture is not parking what it thinks")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())
	if err := depScopeApply(t, svc, "rst", "invoice.atl", restoreDropped, plan); err != nil {
		t.Fatalf("approved drop: %v", err)
	}
}

func parkedRegistrations(t *testing.T, svc *Service) int {
	t.Helper()
	var n int
	if err := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM atlantis.parked_objects
 WHERE parent_table = 'rst_invoice' AND reaped_at IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count registrations: %v", err)
	}
	return n
}

// The recovery the docs promise, for a NOT NULL column: revert the drop, and
// the plan is additive, because the rows come back with the column.
func TestRedeclaringAParkedNotNullFieldRestoresIt(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)

	plan := depScopePlan(t, svc, "rst", "invoice.atl", restoreV1)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_ADDITIVE {
		t.Fatalf("re-declaring the parked columns planned as %s, want additive. "+
			"tide apply refuses a backfill-required plan with no backfill declared, "+
			"so the column cannot be recovered", plan.GetClass())
	}
	if !strings.Contains(plan.GetUpSql(), "restored from its parked column") {
		t.Errorf("the plan does not say the fields are restored:\n%s", plan.GetUpSql())
	}
	if err := depScopeApply(t, svc, "rst", "invoice.atl", restoreV1, plan); err != nil {
		t.Fatalf("apply the restore: %v", err)
	}

	ctx := context.Background()
	var rows, named, coded int
	var name42 string
	if err := svc.pool.QueryRow(ctx, `
SELECT count(*), count(customer_name), count(code),
       max(customer_name) FILTER (WHERE id = 42)
  FROM atlantis.rst_invoice`).Scan(&rows, &named, &coded, &name42); err != nil {
		t.Fatalf("read the restored columns: %v", err)
	}
	if rows != 300 || named != 300 || coded != 300 {
		t.Errorf("rows=%d customer_name=%d code=%d, want 300 of each", rows, named, coded)
	}
	if name42 != "customer 42" {
		t.Errorf("row 42 customer_name = %q, want customer 42", name42)
	}
	var nullable string
	if err := svc.pool.QueryRow(ctx, `
SELECT is_nullable FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'rst_invoice'
   AND column_name = 'customer_name'`).Scan(&nullable); err != nil {
		t.Fatalf("read nullability: %v", err)
	}
	if nullable != "NO" {
		t.Error("the restored column lost its NOT NULL")
	}
	if n := parkedRegistrations(t, svc); n != 0 {
		t.Errorf("%d park registrations left after the restore; the reaper would act on them", n)
	}
}

// A declaration changed from the parked one plans the restore and then each
// difference; the apply ends with the column as declared and the rows kept.
func TestARestoreListsEachDifference(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)
	const changed = `
entity Invoice in rst {
  id            bigint primary
  customer_name varchar(200) not null
  code          varchar(20) unique
}
`
	plan := depScopePlan(t, svc, "rst", "invoice.atl", changed)
	up := plan.GetUpSql()
	restore := strings.Index(up, "RESTORED FROM PARKED")
	for _, want := range []string{"field_type_changed", "field_unique_added"} {
		if i := strings.Index(up, want); i < 0 || i < restore || restore < 0 {
			t.Errorf("%s is missing or runs before the restore:\n%s", want, up)
		}
	}

	ctx := context.Background()
	if _, err := svc.RegisterCaller(ctx, &adminpb.RegisterCallerRequest{
		Caller: "rst", CanMutate: true, CreatedBy: "restore-test",
	}); err != nil {
		t.Fatalf("RegisterCaller: %v", err)
	}
	resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: "rst", Files: depScopeFiles("invoice.atl", changed),
	})
	if err != nil || resp.GetVerdict() != "pass" {
		t.Fatalf("rehearsal: %v, verdict %q (%s)", err, resp.GetVerdict(), resp.GetError())
	}

	applyApproved(t, svc, "rst", "invoice.atl", changed)
	var named int
	var name42, nameType, codeType string
	var uniques int
	if err := svc.pool.QueryRow(ctx, `
SELECT count(customer_name), max(customer_name) FILTER (WHERE id = 42),
       (SELECT format_type(atttypid, atttypmod) FROM pg_attribute
         WHERE attrelid = 'atlantis.rst_invoice'::regclass AND attname = 'customer_name'),
       (SELECT format_type(atttypid, atttypmod) FROM pg_attribute
         WHERE attrelid = 'atlantis.rst_invoice'::regclass AND attname = 'code'),
       (SELECT count(*) FROM pg_constraint
         WHERE conrelid = 'atlantis.rst_invoice'::regclass AND contype = 'u')
  FROM atlantis.rst_invoice`).Scan(&named, &name42, &nameType, &codeType, &uniques); err != nil {
		t.Fatalf("read the restored table: %v", err)
	}
	if named != 300 || name42 != "customer 42" {
		t.Errorf("%d names kept, row 42 %q; want 300 and customer 42", named, name42)
	}
	if nameType != "character varying(200)" || codeType != "character varying(20)" || uniques != 1 {
		t.Errorf("customer_name %s, code %s, %d unique; want varchar(200), varchar(20), 1",
			nameType, codeType, uniques)
	}
}

// A field parked twice is restored as it was at the later park.
func TestARestoreUsesTheLatestPark(t *testing.T) {
	svc := depScopeService(t)
	const (
		narrow = `entity Tag in rsl { id bigint primary  label varchar(10) }`
		wide   = `entity Tag in rsl { id bigint primary  label varchar(20) }`
		gone   = `entity Tag in rsl { id bigint primary }`
	)
	applyApproved(t, svc, "rsl", "t.atl", narrow)
	applyApproved(t, svc, "rsl", "t.atl", gone)
	applyApproved(t, svc, "rsl", "t.atl", narrow)
	applyApproved(t, svc, "rsl", "t.atl", wide)
	applyApproved(t, svc, "rsl", "t.atl", gone)

	plan := depScopePlan(t, svc, "rsl", "t.atl", wide)
	if strings.Contains(plan.GetUpSql(), "field_type_changed") {
		t.Errorf("the restore reads the first park's varchar(10):\n%s", plan.GetUpSql())
	}
	applyApproved(t, svc, "rsl", "t.atl", wide)
}

// A parked column that schema history cannot describe is refused at plan and
// at apply, and the parked rows stay where they are.
func TestARestoreHistoryCannotDescribeIsRefused(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)
	if _, err := svc.pool.Exec(context.Background(), `
ALTER TABLE atlantis.rst_invoice ALTER COLUMN code__parked TYPE varchar(30);
ALTER TABLE atlantis.rst_invoice ADD COLUMN extra__parked text`); err != nil {
		t.Fatalf("out-of-band change: %v", err)
	}

	cases := []struct {
		name, src, want string
	}{
		{"type changed outside atlantis", `
entity Invoice in rst {
  id   bigint primary
  code varchar(10)
}
`, "rst.Invoice.code: its parked column is character varying(30), and schema history records it as VARCHAR(10)"},
		{"no recorded declaration", `
entity Invoice in rst {
  id    bigint primary
  extra text
}
`, "rst.Invoice.extra: a parked column holds this field's rows, and schema history has no declaration for it"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := svc.PlanSchema(context.Background(), &adminpb.PlanSchemaRequest{
				Caller: "rst", Files: depScopeFiles("invoice.atl", c.src),
			})
			if err != nil {
				t.Fatalf("PlanSchema: %v", err)
			}
			if resp.GetClass() != adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE ||
				len(resp.GetParseErrors()) != 1 || !strings.Contains(resp.GetParseErrors()[0], c.want) {
				t.Fatalf("plan: class %s, errors %q; want unparseable with %q",
					resp.GetClass(), resp.GetParseErrors(), c.want)
			}

			// The apply re-reads the parked columns itself, so a client that
			// skips the plan is refused as well.
			err = depScopeApply(t, svc, "rst", "invoice.atl", c.src,
				&adminpb.PlanSchemaResponse{PlanId: "not-a-plan"})
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("apply: %v; want FailedPrecondition with %q", err, c.want)
			}
		})
	}

	if n := parkedRegistrations(t, svc); n != 2 {
		t.Errorf("%d park registrations after the refusals, want 2", n)
	}
}

// A row inserted while customer_name was parked has no value for it. The
// additive restore fails at apply and names the backfill; declaring one plans
// the restore through the backfill path.
func TestARestoreOverRowsInsertedWhileParked(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)
	if _, err := svc.pool.Exec(context.Background(),
		`INSERT INTO atlantis.rst_invoice (id) VALUES (301)`); err != nil {
		t.Fatalf("insert while parked: %v", err)
	}

	plan := depScopePlan(t, svc, "rst", "invoice.atl", restoreV1)
	err := depScopeApply(t, svc, "rst", "invoice.atl", restoreV1, plan)
	if err == nil || !strings.Contains(err.Error(), "Declare a backfill") {
		t.Fatalf("apply over a row with no value: %v; want a refusal naming the backfill", err)
	}

	backfilled := strings.Replace(restoreV1, "customer_name text not null",
		`customer_name text not null backfill "'unknown'"`, 1)
	plan = depScopePlan(t, svc, "rst", "invoice.atl", backfilled)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED {
		t.Fatalf("restore with a backfill planned as %s, want backfill-required", plan.GetClass())
	}
	if !strings.Contains(plan.GetPreBackfillUpSql(), `RENAME COLUMN "customer_name__parked" TO "customer_name"`) {
		t.Errorf("the backfill path does not restore the parked column:\n%s", plan.GetPreBackfillUpSql())
	}
	if n := parkedRegistrations(t, svc); n != 2 {
		t.Errorf("%d park registrations after the refused apply, want 2", n)
	}
}

// A parked enum column restores: the declared type is schema-qualified and
// quoted, and to_regtype and the typed NULL both resolve it.
func TestAParkedEnumColumnRestores(t *testing.T) {
	svc := depScopeService(t)
	const (
		with = `
enum Status in rse { open, paid }

entity Invoice in rse {
  id     bigint primary
  status Status not null
}
`
		without = `
enum Status in rse { open, paid }

entity Invoice in rse {
  id bigint primary
}
`
	)
	depScopePlanAndApply(t, svc, "rse", "invoice.atl", with)
	ctx := context.Background()
	if _, err := svc.pool.Exec(ctx,
		`INSERT INTO atlantis.rse_invoice VALUES (1, 'paid'), (2, 'open')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	drop := depScopePlan(t, svc, "rse", "invoice.atl", without)
	_ = depScopeApply(t, svc, "rse", "invoice.atl", without, drop)
	approveStoredPlan(t, svc, drop.GetPlanId())
	if err := depScopeApply(t, svc, "rse", "invoice.atl", without, drop); err != nil {
		t.Fatalf("approved drop: %v", err)
	}

	plan := depScopePlan(t, svc, "rse", "invoice.atl", with)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_ADDITIVE {
		t.Fatalf("re-declaring the parked enum column planned as %s, want additive", plan.GetClass())
	}
	if err := depScopeApply(t, svc, "rse", "invoice.atl", with, plan); err != nil {
		t.Fatalf("apply the restore: %v", err)
	}
	var status string
	if err := svc.pool.QueryRow(ctx,
		`SELECT status::text FROM atlantis.rse_invoice WHERE id = 1`).Scan(&status); err != nil {
		t.Fatalf("read the restored column: %v", err)
	}
	if status != "paid" {
		t.Errorf("restored status = %q, want paid", status)
	}
}

// A rollback restores a parked column the same way: as its parked
// declaration, then changed to the declaration of the version rolled back to.
func TestARollbackRestoresThroughTheDiff(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	const (
		narrow = `entity Code in rsr { id bigint primary  code varchar(10) }`
		wide   = `entity Code in rsr { id bigint primary  code varchar(20) }`
		gone   = `entity Code in rsr { id bigint primary }`
	)
	latest := func() int64 {
		t.Helper()
		var v int64
		if err := svc.pool.QueryRow(ctx,
			`SELECT max(version) FROM atlantis.schema_versions`).Scan(&v); err != nil {
			t.Fatalf("read version: %v", err)
		}
		return v
	}

	depScopePlanAndApply(t, svc, "rsr", "code.atl", narrow)
	narrowVersion := latest()
	depScopePlanAndApply(t, svc, "rsr", "code.atl", wide)
	applyApproved(t, svc, "rsr", "code.atl", gone)
	if _, err := svc.pool.Exec(ctx, `INSERT INTO atlantis.rsr_code (id, code__parked) VALUES (1, 'ok')`); err != nil {
		t.Fatalf("seed the parked column: %v", err)
	}

	if _, err := svc.RollbackSchema(ctx, &adminpb.RollbackSchemaRequest{ToVersion: narrowVersion, Caller: "rsr"}); err != nil {
		t.Fatalf("rollback to varchar(10): %v", err)
	}
	var typ, code string
	if err := svc.pool.QueryRow(ctx, `
SELECT format_type(a.atttypid, a.atttypmod), c.code
  FROM pg_attribute a, atlantis.rsr_code c
 WHERE a.attrelid = 'atlantis.rsr_code'::regclass AND a.attname = 'code' AND c.id = 1`).Scan(&typ, &code); err != nil {
		t.Fatalf("read the restored column: %v", err)
	}
	if typ != "character varying(10)" || code != "ok" {
		t.Errorf("restored column is %s holding %q, want character varying(10) holding ok", typ, code)
	}
}

// Once the reaper has dropped the parked column, a re-declared NOT NULL field
// is a new, empty column and needs a backfill again. A parked column still
// present restores in the same plan.
func TestARedeclareAfterTheReapIsAFreshColumn(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)
	if _, err := svc.pool.Exec(context.Background(), `
ALTER TABLE atlantis.rst_invoice DROP COLUMN customer_name__parked;
UPDATE atlantis.parked_objects SET reaped_at = now() WHERE object_name = 'customer_name__parked'`); err != nil {
		t.Fatalf("reap: %v", err)
	}

	plan := depScopePlan(t, svc, "rst", "invoice.atl", restoreV1)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED {
		t.Fatalf("re-declaring a reaped NOT NULL column planned as %s, want backfill-required", plan.GetClass())
	}
	if !strings.Contains(plan.GetUpSql(), "field_added: field restored from its parked column") {
		t.Errorf("code, still parked, is not restored in the same plan:\n%s", plan.GetUpSql())
	}
}

// A rehearsal reaches the apply's verdict: the clone carries the parked
// columns, so the restore renames them back there too.
func TestARestoreRehearsesAsItApplies(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)
	ctx := context.Background()
	if _, err := svc.RegisterCaller(ctx, &adminpb.RegisterCallerRequest{
		Caller: "rst", CanMutate: true, CreatedBy: "restore-test",
	}); err != nil {
		t.Fatalf("RegisterCaller: %v", err)
	}
	rehearse := func() *adminpb.RehearseMigrationResponse {
		t.Helper()
		resp, err := svc.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
			Caller: "rst", Files: depScopeFiles("invoice.atl", restoreV1),
		})
		if err != nil {
			t.Fatalf("RehearseMigration: %v", err)
		}
		return resp
	}

	if resp := rehearse(); resp.GetVerdict() != "pass" {
		t.Fatalf("verdict = %q (%s / %s), want pass: the apply restores these columns",
			resp.GetVerdict(), resp.GetReason(), resp.GetError())
	}

	// A row written while customer_name was parked has no value for it.
	if _, err := svc.pool.Exec(ctx, `INSERT INTO atlantis.rst_invoice (id) VALUES (301)`); err != nil {
		t.Fatalf("insert while parked: %v", err)
	}
	resp := rehearse()
	if resp.GetVerdict() != "fail_data" || resp.GetSqlstate() != "23502" {
		t.Fatalf("verdict = %q, sqlstate %q; want fail_data 23502", resp.GetVerdict(), resp.GetSqlstate())
	}
	if !strings.Contains(resp.GetRemediation(), "1 rows inserted into rst_invoice while customer_name was parked") {
		t.Errorf("remediation %q does not count the rows inserted while parked", resp.GetRemediation())
	}
}

// The rehearsal clone gets each parked column with its type and its unique,
// check and foreign-key constraints. A foreign key whose table the clone does
// not have is left out, and the rest still land.
func TestTheRehearsalCloneCarriesParkedConstraints(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	applyApproved(t, svc, "rcc", "t.atl", `
entity Owner in rcc { id bigint primary }
entity Item in rcc {
  id       bigint primary
  code     varchar(10) unique check "length(code) > 1"
  owner_id bigint references rcc.Owner.id
}`)
	applyApproved(t, svc, "rcc", "t.atl", `
entity Owner in rcc { id bigint primary }
entity Item in rcc { id bigint primary }`)

	parked, err := readParkedColumns(ctx, svc.pool)
	if err != nil {
		t.Fatalf("readParkedColumns: %v", err)
	}
	if len(parked) != 2 {
		t.Fatalf("read %d parked columns, want 2: %+v", len(parked), parked)
	}

	cases := []struct {
		name, db, tables string
		want             []string
	}{
		{"with the referenced table", "atlantis_rcc_clone", `
CREATE TABLE atlantis.rcc_owner (id bigint PRIMARY KEY);
CREATE TABLE atlantis.rcc_item (id bigint PRIMARY KEY)`,
			[]string{"CHECK ((length((code__parked)::text) > 1))",
				"FOREIGN KEY (owner_id__parked) REFERENCES atlantis.rcc_owner(id)",
				"UNIQUE (code__parked)"}},
		{"without it", "atlantis_rcc_clone_noref", `
CREATE TABLE atlantis.rcc_item (id bigint PRIMARY KEY)`,
			[]string{"CHECK ((length((code__parked)::text) > 1))", "UNIQUE (code__parked)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, err := pgx.Connect(ctx, pgcatalog.PrivateDatabase(t, os.Getenv("ATLANTIS_TEST_PG"), c.db))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = conn.Close(context.Background()) })
			if _, err := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS atlantis;"+c.tables); err != nil {
				t.Fatalf("clone tables: %v", err)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := addParkedColumns(ctx, tx, parked); err != nil {
				t.Fatalf("addParkedColumns: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}

			if _, err := conn.Exec(ctx, `SET search_path = pg_catalog`); err != nil {
				t.Fatalf("search_path: %v", err)
			}
			rows, err := conn.Query(ctx, `
SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid = 'atlantis.rcc_item'::regclass AND contype <> 'p' ORDER BY 1`)
			if err != nil {
				t.Fatalf("read constraints: %v", err)
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("clone constraints:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

// A database without atlantis.parked_objects, such as a managed database the
// infra migrations never ran on, still reads its parked columns: by the
// `__parked` suffix alone.
func TestParkedColumnsWithoutTheRegistry(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to read parked columns without the registry")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_noregistry"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `
CREATE SCHEMA atlantis;
CREATE TABLE atlantis.nr_t (id bigint PRIMARY KEY, note__parked text CHECK (note__parked <> ''),
  CONSTRAINT nr_t_both CHECK (id > 0 OR note__parked IS NULL))`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	parked, err := readParkedColumns(ctx, conn)
	if err != nil {
		t.Fatalf("readParkedColumns: %v", err)
	}
	if len(parked) != 1 || parked[0].column != "note__parked" || len(parked[0].constraints) != 1 {
		t.Errorf("parked columns %+v; want note__parked with its check", parked)
	}

	// A check on a live column as well is not the parked column's alone.
	drift := []introspect.CheckConstraintDrift{
		{Kind: introspect.CheckLiveNotDeclared, Schema: "atlantis", Table: "nr_t", ConstraintName: "nr_t_note__parked_check"},
		{Kind: introspect.CheckLiveNotDeclared, Schema: "atlantis", Table: "nr_t", ConstraintName: "nr_t_both"},
	}
	kept, err := withoutParkedChecks(ctx, conn, drift)
	if err != nil || len(kept) != 1 || kept[0].ConstraintName != "nr_t_both" {
		t.Errorf("withoutParkedChecks: %v, kept %+v; want nr_t_both alone", err, kept)
	}
}

// countingQuerier counts the queries a diff makes.
type countingQuerier struct {
	q introspect.Querier
	n int
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.q.Query(ctx, sql, args...)
}

// One query when a field is added. A parked column adds a read of history and
// two type queries, and the typmod query only when the base type matches.
func TestDiffWithParkedQueryCount(t *testing.T) {
	svc := depScopeService(t)
	restoreParked(t, svc)

	lowered := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("invoice.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	prior := lowered(restoreDropped)
	check := func(name, src string, wantDB, wantHistory int) {
		t.Helper()
		db, history := &countingQuerier{q: svc.pool}, &countingQuerier{q: svc.pool}
		next := lowered(src)
		codegen.AssignProtoNumbers(prior, next)
		if _, _, err := diffWithParked(context.Background(), db, history, prior, next); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if db.n != wantDB || history.n != wantHistory {
			t.Errorf("%s: %d database and %d history queries, want %d and %d",
				name, db.n, history.n, wantDB, wantHistory)
		}
	}
	check("no parked column", `entity Invoice in rst { id bigint primary  memo text }`, 1, 0)
	check("parked column", `entity Invoice in rst { id bigint primary  customer_name text not null }`, 3, 1)

	if _, err := svc.pool.Exec(context.Background(),
		`ALTER TABLE atlantis.rst_invoice ALTER COLUMN customer_name__parked TYPE bigint USING 0`); err != nil {
		t.Fatalf("out-of-band change: %v", err)
	}
	check("parked column of another base type", `entity Invoice in rst { id bigint primary  customer_name text not null }`, 2, 1)
}
