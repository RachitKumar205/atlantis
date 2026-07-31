package codegen

import (
	"context"
	"os"
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
		_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS atlantis_tombstone CASCADE`)
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
		_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS atlantis_tombstone CASCADE`)
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
		`SELECT notes FROM atlantis_tombstone.prk_memo__parked WHERE id = 1`).Scan(&body); err != nil {
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
