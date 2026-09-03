package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Re-declaring a dropped object restores the parked original, rows and
// all. These tests execute the emitted DDL: the restore is a DO block
// whose decision runs in the database, so only the database can say
// whether it decided correctly.

func unparkConn(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the unpark DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn, ctx
}

func unparkIR(extraColumn bool) *dsl.IR {
	fields := []dsl.Field{
		{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
		{Name: "body", Type: dsl.FieldType{Name: "text"}, NotNull: true},
	}
	if extraColumn {
		fields = append(fields, dsl.Field{Name: "extra", Type: dsl.FieldType{Name: "text"}})
	}
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Note", Namespace: "unp", Fields: fields,
	}}}
}

func unparkReset(t *testing.T, conn *pgx.Conn, ctx context.Context) {
	t.Helper()
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.unp_note CASCADE`)
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis_tombstone.atlantis_unp_note__parked`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name IN ('unp_note', 'body')`)
	}
	reset()
	t.Cleanup(reset)
}

func unparkOwner() DiffOption {
	return WithCallerContext("backend", map[string]string{"unp.Note": "backend"}, nil)
}

// The recovery path the docs and the console promise: revert the drop,
// apply, and the table is back with the rows it held.
func TestRedeclaringAParkedTableRestoresItsRows(t *testing.T) {
	conn, ctx := unparkConn(t)
	unparkReset(t, conn, ctx)

	with, empty := unparkIR(false), &dsl.IR{}
	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.unp_note VALUES (1, 'irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	drop := ComputeDiff(with, empty, unparkOwner())
	parkMig, err := EmitSQL(with, empty, drop)
	if err != nil {
		t.Fatalf("EmitSQL park: %v", err)
	}
	if _, err := conn.Exec(ctx, parkMig.Up); err != nil {
		t.Fatalf("park migration: %v\n%s", err, parkMig.Up)
	}

	readd := ComputeDiff(empty, with, unparkOwner())
	restoreMig, err := EmitSQL(empty, with, readd)
	if err != nil {
		t.Fatalf("EmitSQL restore: %v", err)
	}
	if _, err := conn.Exec(ctx, restoreMig.Up); err != nil {
		t.Fatalf("restore migration: %v\n%s", err, restoreMig.Up)
	}

	var body string
	if err := conn.QueryRow(ctx,
		`SELECT body FROM atlantis.unp_note WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("the re-declared table does not hold the parked rows: %v", err)
	}
	if body != "irreplaceable" {
		t.Errorf("restored value = %q, want irreplaceable", body)
	}

	// The tombstone is gone, and with it the reaper's claim: a stale
	// registration would let the reaper drop the restored table's name
	// out of parked_objects bookkeeping it no longer owns.
	var tombstones int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM atlantis.parked_objects
 WHERE original_name = 'unp_note' AND reaped_at IS NULL`).Scan(&tombstones); err != nil {
		t.Fatalf("count registrations: %v", err)
	}
	if tombstones != 0 {
		t.Error("the restore left the park registration behind; the reaper would act on it")
	}
}

// A tombstone with a different column set is not silently restored and
// not silently shadowed by a fresh table: the apply is refused with the
// way out in the message.
func TestRedeclaringWithDifferentColumnsIsRefused(t *testing.T) {
	conn, ctx := unparkConn(t)
	unparkReset(t, conn, ctx)

	with, empty := unparkIR(false), &dsl.IR{}
	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.unp_note VALUES (1, 'held')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	drop := ComputeDiff(with, empty, unparkOwner())
	parkMig, err := EmitSQL(with, empty, drop)
	if err != nil {
		t.Fatalf("EmitSQL park: %v", err)
	}
	if _, err := conn.Exec(ctx, parkMig.Up); err != nil {
		t.Fatalf("park migration: %v", err)
	}

	changed := unparkIR(true)
	readd := ComputeDiff(empty, changed, unparkOwner())
	restoreMig, err := EmitSQL(empty, changed, readd)
	if err != nil {
		t.Fatalf("EmitSQL restore: %v", err)
	}
	_, execErr := conn.Exec(ctx, restoreMig.Up)
	if execErr == nil {
		t.Fatal("a re-declare with a different column set was not refused")
	}
	if !strings.Contains(execErr.Error(), "different column set") {
		t.Errorf("the refusal does not name the cause: %v", execErr)
	}

	// The parked rows are untouched by the refusal.
	var body string
	if err := conn.QueryRow(ctx,
		`SELECT body FROM atlantis_tombstone.atlantis_unp_note__parked WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("the refusal disturbed the tombstone: %v", err)
	}
	if body != "held" {
		t.Errorf("tombstone value = %q, want held", body)
	}
}

// Re-declaring a dropped field renames the parked column back, data and
// NOT NULL included.
func TestRedeclaringAParkedColumnRestoresItsData(t *testing.T) {
	conn, ctx := unparkConn(t)
	unparkReset(t, conn, ctx)

	with, without := parkIR(true), parkIR(false)
	// parkIR's entity is prk.Memo; reuse its cleanup shape.
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.prk_memo CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE original_name = 'notes'`)
	}
	reset()
	t.Cleanup(reset)

	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`INSERT INTO atlantis.prk_memo VALUES (1, 'kept', 'irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	drop := ComputeDiff(with, without, parkOwner())
	parkMig, err := EmitSQL(with, without, drop)
	if err != nil {
		t.Fatalf("EmitSQL park: %v", err)
	}
	if _, err := conn.Exec(ctx, parkMig.Up); err != nil {
		t.Fatalf("park migration: %v", err)
	}

	readd := ComputeDiff(without, with, parkOwner())
	restoreMig, err := EmitSQL(without, with, readd)
	if err != nil {
		t.Fatalf("EmitSQL restore: %v", err)
	}
	if _, err := conn.Exec(ctx, restoreMig.Up); err != nil {
		t.Fatalf("restore migration: %v\n%s", err, restoreMig.Up)
	}

	var notes string
	if err := conn.QueryRow(ctx,
		`SELECT notes FROM atlantis.prk_memo WHERE id = 1`).Scan(&notes); err != nil {
		t.Fatalf("the re-declared column does not hold the parked data: %v", err)
	}
	if notes != "irreplaceable" {
		t.Errorf("restored value = %q, want irreplaceable", notes)
	}

	// The declared NOT NULL came back with the rename.
	var nullable string
	if err := conn.QueryRow(ctx, `
SELECT is_nullable FROM information_schema.columns
 WHERE table_schema='atlantis' AND table_name='prk_memo' AND column_name='notes'`).Scan(&nullable); err != nil {
		t.Fatalf("read nullability: %v", err)
	}
	if nullable != "NO" {
		t.Error("the restored column lost its NOT NULL")
	}

	var registrations int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM atlantis.parked_objects
 WHERE original_name = 'notes' AND reaped_at IS NULL`).Scan(&registrations); err != nil {
		t.Fatalf("count registrations: %v", err)
	}
	if registrations != 0 {
		t.Error("the restore left the column's park registration behind")
	}
}
