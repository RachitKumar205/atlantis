package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Destructive changes are parked, not dropped, and the property that matters is
// that the rows are still there afterwards.
//
// Before this, deleting a line from a .atl file and running `tide apply`
// classified as ADDITIVE — auto-applied, unattended — and emitted
// `ALTER TABLE ... DROP COLUMN` or `DROP TABLE ... CASCADE`. The recovery path
// the documentation offered was point-in-time restore of the whole database,
// which is not a recovery path for one mistaken line in a schema file.
//
// Asserting on the DDL text cannot establish this. Only reading the rows back
// after the migration can.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/codegen/ -run Park -v

func parkIR(withNotes bool) *dsl.IR {
	fields := []dsl.Field{
		{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
		{Name: "keep", Type: dsl.FieldType{Name: "text"}, NotNull: true},
	}
	if withNotes {
		fields = append(fields, dsl.Field{
			Name: "notes", Type: dsl.FieldType{Name: "text"}, NotNull: true,
		})
	}
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Memo", Namespace: "prk", Fields: fields,
	}}}
}

func parkOwner() DiffOption {
	return WithCallerContext("backend", map[string]string{"prk.Memo": "backend"}, nil)
}

func TestParkedColumnKeepsItsData(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the park DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.prk_memo CASCADE`)
		// Only this test's own tombstone, never the schema: it is shared, and
		// `go test ./...` runs packages concurrently, so dropping the schema
		// destroys the reaper package's fixtures mid-run.
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.atlantis_prk_memo__parked`)
	}
	reset()
	t.Cleanup(reset)

	with, without := parkIR(true), parkIR(false)
	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.prk_memo VALUES (1,'kept','irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	d := ComputeDiff(with, without, parkOwner())

	// Removing a column the caller owns is destructive, not additive. Additive
	// is what `tide apply` applies unattended.
	if len(d.Destructive) != 1 {
		t.Fatalf("dropping an owned column produced %d destructive changes, want 1. "+
			"additive=%d breaking=%d — additive means auto-applied with no gate",
			len(d.Destructive), len(d.Additive), len(d.Breaking))
	}
	if d.HighestClass() != ClassDestructive {
		t.Errorf("HighestClass = %v, want destructive", d.HighestClass())
	}

	mig, err := EmitSQL(with, without, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("park migration failed:\n%s\n  %v", mig.Up, err)
	}

	// The column is gone from the application's point of view: every generated
	// statement names its columns, so a renamed column is absent from reads,
	// writes and the proto surface.
	var liveCols int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema='atlantis' AND table_name='prk_memo' AND column_name='notes'`).Scan(&liveCols); err != nil {
		t.Fatalf("query: %v", err)
	}
	if liveCols != 0 {
		t.Error("the column is still present under its original name; nothing was parked")
	}

	// And the data is still there. This is the whole point.
	var parked string
	if err := conn.QueryRow(ctx,
		`SELECT notes__parked FROM atlantis.prk_memo WHERE id = 1`).Scan(&parked); err != nil {
		t.Fatalf("the parked column is not readable, so the data is gone: %v", err)
	}
	if parked != "irreplaceable" {
		t.Errorf("parked value = %q, want irreplaceable", parked)
	}

	// A parked NOT NULL column must not reject writes to the live ones.
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.prk_memo (id, keep) VALUES (2,'still works')`); err != nil {
		t.Errorf("a parked NOT NULL column blocked an insert into the live columns: %v", err)
	}

	// Undoing it is a rename back, with the rows intact — the property that
	// makes this a recovery path rather than a slower deletion.
	if _, err := conn.Exec(ctx,
		`ALTER TABLE atlantis.prk_memo RENAME COLUMN notes__parked TO notes`); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var restored string
	if err := conn.QueryRow(ctx,
		`SELECT notes FROM atlantis.prk_memo WHERE id = 1`).Scan(&restored); err != nil {
		t.Fatalf("restored read: %v", err)
	}
	if restored != "irreplaceable" {
		t.Errorf("restored value = %q, want irreplaceable", restored)
	}
}

func TestParkedTableKeepsItsData(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the park DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.prk_memo CASCADE`)
		// Only this test's own tombstone, never the schema: it is shared, and
		// `go test ./...` runs packages concurrently, so dropping the schema
		// destroys the reaper package's fixtures mid-run.
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.atlantis_prk_memo__parked`)
	}
	reset()
	t.Cleanup(reset)

	with := parkIR(true)
	initial, _ := EmitInitial(with)
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.prk_memo VALUES (1,'kept','irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	empty := &dsl.IR{}
	d := ComputeDiff(with, empty, parkOwner())
	if len(d.Destructive) != 1 {
		t.Fatalf("dropping an owned entity produced %d destructive changes, want 1 "+
			"(additive=%d)", len(d.Destructive), len(d.Additive))
	}

	mig, err := EmitSQL(with, empty, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	// CASCADE is gone. It removed dependent objects that were never part of the
	// plan, so the blast radius exceeded what the plan showed.
	if contains(mig.Up, "CASCADE") {
		t.Errorf("the drop path still uses CASCADE:\n%s", mig.Up)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("park migration failed:\n%s\n  %v", mig.Up, err)
	}

	// Gone from the live schema...
	var live int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM information_schema.tables
 WHERE table_schema='atlantis' AND table_name='prk_memo'`).Scan(&live); err != nil {
		t.Fatalf("query: %v", err)
	}
	if live != 0 {
		t.Error("the table is still in the live schema; nothing was parked")
	}

	// ...and still readable, with its rows, in the tombstone schema.
	var body string
	if err := conn.QueryRow(ctx,
		`SELECT notes FROM atlantis_tombstone.atlantis_prk_memo__parked WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("the parked table is not readable, so the data is gone: %v", err)
	}
	if body != "irreplaceable" {
		t.Errorf("parked value = %q, want irreplaceable", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Parking must register itself, in the same transaction as the rename.
//
// A park applied but not recorded is invisible to the reaper and to anyone
// looking for it: the object survives every retention window and no list says
// it exists. The registration is emitted into the migration for exactly that
// reason, so the two cannot come apart.
func TestParkRegistersItselfForReaping(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the park DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.prk_memo CASCADE`)
		// Only this test's own tombstone, never the schema: it is shared, and
		// `go test ./...` runs packages concurrently, so dropping the schema
		// destroys the reaper package's fixtures mid-run.
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.atlantis_prk_memo__parked`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name IN ('notes','prk_memo')`)
	}
	reset()
	t.Cleanup(reset)

	with, without := parkIR(true), parkIR(false)
	initial, _ := EmitInitial(with)
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	mig, err := EmitSQL(with, without, ComputeDiff(with, without, parkOwner()))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("park: %v\n%s", err, mig.Up)
	}

	var kind, schemaName, object, parent string
	var reapAfterFuture bool
	if err := conn.QueryRow(ctx, `
SELECT kind, schema_name, object_name, coalesce(parent_table,''), reap_after > now()
  FROM atlantis.parked_objects
 WHERE original_name = 'notes' AND reaped_at IS NULL`).Scan(
		&kind, &schemaName, &object, &parent, &reapAfterFuture); err != nil {
		t.Fatalf("the park was applied but never registered, so nothing will ever "+
			"reap it and no list says it exists: %v", err)
	}
	// All four, because the reaper interpolates all four into its DDL. schemaName
	// was previously scanned and never checked, so a wrong schema meant every
	// reap attempt failed forever — or found a same-named column somewhere else
	// and dropped that instead.
	if kind != "column" || schemaName != "atlantis" || parent != "prk_memo" || object != "notes__parked" {
		t.Errorf("registration = kind %q schema %q parent %q object %q; the reaper "+
			"addresses a column through all four, so any of them being wrong reaps "+
			"nothing or the wrong thing", kind, schemaName, parent, object)
	}
	if !reapAfterFuture {
		t.Error("reap_after is not in the future, so the object is due immediately " +
			"and the retention window is no window at all")
	}

	// Undoing the park must also remove the registration — the object was
	// restored, not reaped, and a stale row would have the reaper drop a live
	// column once the window passed.
	if _, err := conn.Exec(ctx, mig.Down); err != nil {
		t.Fatalf("unpark: %v\n%s", err, mig.Down)
	}
	var still int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM atlantis.parked_objects
 WHERE original_name = 'notes' AND reaped_at IS NULL`).Scan(&still); err != nil {
		t.Fatalf("count: %v", err)
	}
	if still != 0 {
		t.Error("restoring the column left its parked registration behind. The " +
			"reaper would drop the restored, live column when the window passed")
	}
}

// adoptedIR is an entity that overrides its physical table, which is what a
// database adopted with `tidectl adopt` looks like. 17 of the 19 .atl files in
// this repo declare `table "..."`, so this is the common case, not the exotic
// one.
func adoptedIR(present bool) *dsl.IR {
	if !present {
		return &dsl.IR{}
	}
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Ledger", Namespace: "prk", TableName: "prk_adopted.ledger",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "amount", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
		},
	}}}
}

// Parking a table whose entity overrides its physical name must park THAT
// table, register it, and restore it to the schema it came from.
//
// This is driven end to end because the failure it guards against was
// invisible at every smaller granularity: the emitted SQL applied without
// error, the migration reported success, and the register gained a row. The
// table was in the tombstone schema under a name nobody had written down, the
// registration pointed at a name that had never existed, the reaper's DROP
// found nothing and recorded a successful reap, and the down migration
// restored nothing while reporting success. Only executing the whole sequence
// and then looking for the rows shows it.
func TestParkingAnAdoptedTableKeepsAndRestoresIt(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the park DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	reset := func() {
		_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS prk_adopted CASCADE`)
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.prk_adopted_ledger__parked`)
		// The bare name too: a park that mis-resolves leaves the real table in
		// the tombstone schema under its original name, and one such run would
		// otherwise poison every later run of this test.
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.ledger`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name = 'ledger'`)
	}
	reset()
	t.Cleanup(reset)

	if _, err := conn.Exec(ctx, `CREATE SCHEMA prk_adopted`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	with, without := adoptedIR(true), adoptedIR(false)
	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO prk_adopted.ledger (id, amount) VALUES (1, 4200)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	owner := WithCallerContext("backend", map[string]string{"prk.Ledger": "backend"}, nil)
	mig, err := EmitSQL(with, without, ComputeDiff(with, without, owner))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("park: %v\n%s", err, mig.Up)
	}

	// The rows must exist, in the tombstone, under the registered name. Reading
	// them through the register is the point: if the register cannot find the
	// data, neither can the reaper or an operator.
	var schemaName, object, origSchema, origName string
	if err := conn.QueryRow(ctx, `
SELECT schema_name, object_name, original_schema, original_name
  FROM atlantis.parked_objects
 WHERE kind = 'table' AND original_name = 'ledger' AND reaped_at IS NULL`).Scan(
		&schemaName, &object, &origSchema, &origName); err != nil {
		t.Fatalf("the parked table was never registered: %v", err)
	}
	if origSchema != "prk_adopted" {
		t.Errorf("original_schema = %q, want prk_adopted. A restore cannot know "+
			"where to put the table back", origSchema)
	}

	var amount int64
	if err := conn.QueryRow(ctx, `SELECT amount FROM `+
		pgIdent(schemaName)+`.`+pgIdent(object)+` WHERE id = 1`).Scan(&amount); err != nil {
		t.Fatalf("the register names %s.%s, and there is no such table with the "+
			"parked rows in it. The real table is stranded somewhere the register "+
			"does not describe: %v", schemaName, object, err)
	}
	if amount != 4200 {
		t.Errorf("parked row lost its data: amount = %d, want 4200", amount)
	}

	// Nothing may be left behind in the source schema.
	var stillThere bool
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('prk_adopted.ledger') IS NOT NULL`).Scan(&stillThere); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if stillThere {
		t.Error("the table is still in its original schema, so the park did not " +
			"make it invisible to the application")
	}

	// And the down migration must put it back where it came from, with its rows.
	if _, err := conn.Exec(ctx, mig.Down); err != nil {
		t.Fatalf("unpark: %v\n%s", err, mig.Down)
	}
	if err := conn.QueryRow(ctx,
		`SELECT amount FROM prk_adopted.ledger WHERE id = 1`).Scan(&amount); err != nil {
		t.Fatalf("the down migration reported success and the table is not back "+
			"in prk_adopted: %v", err)
	}
	if amount != 4200 {
		t.Errorf("restored row lost its data: amount = %d, want 4200", amount)
	}
	var live int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM atlantis.parked_objects
	  WHERE original_name = 'ledger' AND reaped_at IS NULL`).Scan(&live); err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 0 {
		t.Error("the restored table is still registered as parked. The reaper " +
			"would drop it, live and in use, once the window passed")
	}
}

// Restoring a table the reaper has already dropped must fail loudly and say
// why. Both statements in the unpark are renames, and a rename of an absent
// table under IF EXISTS is a silent no-op — so this path used to report
// success having restored nothing at all.
func TestUnparkingAReapedTableFailsLoudly(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the park DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	reset := func() {
		_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS prk_adopted CASCADE`)
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.prk_adopted_ledger__parked`)
		// The bare name too: a park that mis-resolves leaves the real table in
		// the tombstone schema under its original name, and one such run would
		// otherwise poison every later run of this test.
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.ledger`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name = 'ledger'`)
	}
	reset()
	t.Cleanup(reset)

	if _, err := conn.Exec(ctx, `CREATE SCHEMA prk_adopted`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	with, without := adoptedIR(true), adoptedIR(false)
	initial, _ := EmitInitial(with)
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	owner := WithCallerContext("backend", map[string]string{"prk.Ledger": "backend"}, nil)
	mig, err := EmitSQL(with, without, ComputeDiff(with, without, owner))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Simulate the reaper having done its job after the retention window.
	if _, err := conn.Exec(ctx,
		`DROP TABLE atlantis_tombstone.prk_adopted_ledger__parked`); err != nil {
		t.Fatalf("simulated reap: %v", err)
	}

	_, err = conn.Exec(ctx, mig.Down)
	if err == nil {
		t.Fatal("restoring a table that was already reaped reported SUCCESS. The " +
			"operator is told the rollback worked and the data is gone")
	}
	if !strings.Contains(err.Error(), "retention window") {
		t.Errorf("the failure does not explain itself. An operator sees only a "+
			"rename error and has to work out that the reaper took it: %v", err)
	}
}

// pgIdent double-quotes an identifier for use in a test query.
func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
