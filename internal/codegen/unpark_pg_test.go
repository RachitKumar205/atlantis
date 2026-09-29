package codegen

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rachitkumar205/atlantis/internal/backfill"
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

// unparkExec runs one emitted script, as migrate does.
func unparkExec(t *testing.T, conn *pgx.Conn, ctx context.Context, from, to *dsl.IR, opts ...DiffOption) error {
	t.Helper()
	scripts, err := EmitSQL(from, to, ComputeDiff(from, to, opts...))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	_, err = conn.Exec(ctx, scripts.Up)
	return err
}

// A parked column of another type stays parked: restoring it would give the
// field a type its declaration does not have.
func TestRedeclaringAParkedColumnAsAnotherTypeIsRefused(t *testing.T) {
	conn, ctx := unparkConn(t)
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.urt_memo CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'urt_memo'`)
	}
	reset()
	t.Cleanup(reset)

	memo := func(notes string) *dsl.IR {
		fields := []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}
		if notes != "" {
			fields = append(fields, dsl.Field{Name: "notes", Type: dsl.FieldType{Name: notes}})
		}
		return &dsl.IR{Entities: []dsl.Entity{{Name: "Memo", Namespace: "urt", Fields: fields}}}
	}
	owner := WithCallerContext("backend", map[string]string{"urt.Memo": "backend"}, nil)

	initial, err := EmitInitial(memo("text"))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.urt_memo VALUES (1, 'irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := unparkExec(t, conn, ctx, memo("text"), memo(""), owner); err != nil {
		t.Fatalf("park migration: %v", err)
	}

	err = unparkExec(t, conn, ctx, memo(""), memo("bigint"), owner)
	if err == nil {
		t.Fatal("a parked text column was restored under a bigint declaration")
	}
	if !strings.Contains(err.Error(), "is text, and the declaration says BIGINT") {
		t.Errorf("the refusal does not name both types: %v", err)
	}

	var notes string
	if err := conn.QueryRow(ctx,
		`SELECT notes__parked FROM atlantis.urt_memo WHERE id = 1`).Scan(&notes); err != nil {
		t.Fatalf("the refusal disturbed the parked column: %v", err)
	}
	if notes != "irreplaceable" {
		t.Errorf("parked value = %q, want irreplaceable", notes)
	}
}

// A row inserted while a NOT NULL column was parked has no value for it. The
// plain restore refuses with the table and column set, which a rehearsal
// counts by; the backfill path restores the column, keeps every parked value,
// and fills only that row.
func TestRestoringOverRowsInsertedWhileParked(t *testing.T) {
	conn, ctx := unparkConn(t)
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.urb_memo CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'urb_memo'`)
	}
	reset()
	t.Cleanup(reset)

	memo := func(notes *dsl.Field) *dsl.IR {
		fields := []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}
		if notes != nil {
			fields = append(fields, *notes)
		}
		return &dsl.IR{Entities: []dsl.Entity{{Name: "Memo", Namespace: "urb", Fields: fields}}}
	}
	plain := &dsl.Field{Name: "notes", Type: dsl.FieldType{Name: "text"}, NotNull: true}
	backfilled := &dsl.Field{Name: "notes", Type: dsl.FieldType{Name: "text"}, NotNull: true,
		Backfill: "'filled'"}
	owner := WithCallerContext("backend", map[string]string{"urb.Memo": "backend"}, nil)
	restoring := WithParkedFields(map[string]*dsl.Field{"urb.Memo.notes": plain})

	initial, err := EmitInitial(memo(plain))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.urb_memo VALUES (1, 'irreplaceable')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := unparkExec(t, conn, ctx, memo(plain), memo(nil), owner); err != nil {
		t.Fatalf("park migration: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.urb_memo (id) VALUES (2)`); err != nil {
		t.Fatalf("insert while parked: %v", err)
	}

	err = unparkExec(t, conn, ctx, memo(nil), memo(plain), restoring)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Fatalf("restore over a row with no value: %v, want SQLSTATE 23502", err)
	}
	if pgErr.TableName != "urb_memo" || pgErr.ColumnName != "notes" ||
		!strings.Contains(pgErr.Message, "Declare a backfill") {
		t.Errorf("table %q column %q message %q; want urb_memo, notes, and the way out",
			pgErr.TableName, pgErr.ColumnName, pgErr.Message)
	}

	d := ComputeDiff(memo(nil), memo(backfilled), restoring)
	scripts, err := EmitSQL(memo(nil), memo(backfilled), d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if scripts.PreBackfillUp == "" || len(scripts.BackfillFields) != 1 {
		t.Fatalf("no phase split for a restored field with a backfill:\n%s", scripts.Up)
	}
	if _, err := conn.Exec(ctx, scripts.PreBackfillUp); err != nil {
		t.Fatalf("pre-backfill: %v\n%s", err, scripts.PreBackfillUp)
	}
	f := scripts.BackfillFields[0]
	if _, err := conn.Exec(ctx, backfill.ChunkSQL(f.TableName, f.PKColumn, f.Field, f.Expression), 0, 1000); err != nil {
		t.Fatalf("backfill chunk: %v", err)
	}
	if _, err := conn.Exec(ctx, scripts.PostBackfillUp); err != nil {
		t.Fatalf("post-backfill: %v\n%s", err, scripts.PostBackfillUp)
	}

	got := map[int64]string{}
	rows, err := conn.Query(ctx, `SELECT id, notes FROM atlantis.urb_memo ORDER BY id`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for rows.Next() {
		var id int64
		var notes string
		if err := rows.Scan(&id, &notes); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = notes
	}
	if got[1] != "irreplaceable" || got[2] != "filled" || len(got) != 2 {
		t.Errorf("rows = %v; want 1 irreplaceable (restored) and 2 filled (backfilled)", got)
	}
}

// parkDoc creates urd.Doc from v1, writes a row, and parks every field but
// id. It returns the IR with only id and the parked declarations.
func parkDoc(t *testing.T, conn *pgx.Conn, ctx context.Context, v1, seed string) (*dsl.IR, map[string]*dsl.Field) {
	t.Helper()
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.urd_doc CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'urd_doc'`)
	}
	reset()
	t.Cleanup(reset)

	with := lower(t, v1)
	without := lower(t, `entity Doc in urd { id bigint primary }`)
	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	owner := WithCallerContext("backend", map[string]string{"urd.Doc": "backend"}, nil)
	if err := unparkExec(t, conn, ctx, with, without, owner); err != nil {
		t.Fatalf("park migration: %v", err)
	}
	parked := map[string]*dsl.Field{}
	for i := range with.Entities[0].Fields {
		if f := &with.Entities[0].Fields[i]; f.Name != "id" {
			parked["urd.Doc."+f.Name] = f
		}
	}
	return without, parked
}

// A restore whose declaration changed ends with the column as declared: each
// difference runs after the rename, and the rows survive.
func TestARestoreAppliesEachDifference(t *testing.T) {
	conn, ctx := unparkConn(t)
	without, parked := parkDoc(t, conn, ctx, `
entity Doc in urd {
  id    bigint primary
  title text not null
  code  varchar(10) unique
  score int check "score > 0"
  note  text default "x"
}`, `INSERT INTO atlantis.urd_doc VALUES (1, 'kept', 'C1', 5, 'n')`)

	v2 := lower(t, `
entity Doc in urd {
  id    bigint primary
  title varchar(200) not null
  code  varchar(20)
  score int check "score >= 0"
  note  text default "y"
}`)
	d := ComputeDiff(without, v2, WithParkedFields(parked))
	scripts, err := EmitSQL(without, v2, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("restore with differences: %v\n%s", err, scripts.Up)
	}

	var title, code, titleType, codeType, check, def string
	var uniques int
	if err := conn.QueryRow(ctx, `
SELECT d.title, d.code,
       format_type((SELECT atttypid FROM pg_attribute WHERE attrelid = 'atlantis.urd_doc'::regclass AND attname = 'title'),
                   (SELECT atttypmod FROM pg_attribute WHERE attrelid = 'atlantis.urd_doc'::regclass AND attname = 'title')),
       format_type((SELECT atttypid FROM pg_attribute WHERE attrelid = 'atlantis.urd_doc'::regclass AND attname = 'code'),
                   (SELECT atttypmod FROM pg_attribute WHERE attrelid = 'atlantis.urd_doc'::regclass AND attname = 'code')),
       (SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'atlantis.urd_doc'::regclass AND contype = 'c'),
       (SELECT pg_get_expr(adbin, adrelid) FROM pg_attrdef WHERE adrelid = 'atlantis.urd_doc'::regclass
           AND adnum = (SELECT attnum FROM pg_attribute WHERE attrelid = 'atlantis.urd_doc'::regclass AND attname = 'note')),
       (SELECT count(*) FROM pg_constraint WHERE conrelid = 'atlantis.urd_doc'::regclass AND contype = 'u')
  FROM atlantis.urd_doc d WHERE d.id = 1`).Scan(&title, &code, &titleType, &codeType, &check, &def, &uniques); err != nil {
		t.Fatalf("read the restored table: %v", err)
	}
	if title != "kept" || code != "C1" {
		t.Errorf("restored values %q, %q; want kept, C1", title, code)
	}
	if titleType != "character varying(200)" || codeType != "character varying(20)" {
		t.Errorf("types %s, %s; want character varying(200), character varying(20)", titleType, codeType)
	}
	if !strings.Contains(check, ">= 0") {
		t.Errorf("check %q; want the new predicate", check)
	}
	if def != "'y'::text" {
		t.Errorf("default %q; want 'y'::text", def)
	}
	if uniques != 0 {
		t.Errorf("%d unique constraints; the declaration dropped code's", uniques)
	}

	// The down script undoes each difference, then parks the columns again
	// with their rows.
	if _, err := conn.Exec(ctx, scripts.Down); err != nil {
		t.Fatalf("down: %v\n%s", err, scripts.Down)
	}
	var parkedTitle string
	var registrations int
	if err := conn.QueryRow(ctx, `
SELECT title__parked, (SELECT count(*) FROM atlantis.parked_objects
                        WHERE parent_table = 'urd_doc' AND reaped_at IS NULL)
  FROM atlantis.urd_doc WHERE id = 1`).Scan(&parkedTitle, &registrations); err != nil {
		t.Fatalf("read the parked columns after down: %v", err)
	}
	if parkedTitle != "kept" || registrations != 4 {
		t.Errorf("after down: title__parked %q, %d registrations; want kept and 4", parkedTitle, registrations)
	}
}

// The reaper takes no apply lock. A restore whose parked column it dropped
// after the plan raises, where an ADD COLUMN would bring back an empty one.
func TestARestoreRaisesWhenItsColumnIsGone(t *testing.T) {
	conn, ctx := unparkConn(t)
	without, parked := parkDoc(t, conn, ctx, `
entity Doc in urd {
  id    bigint primary
  title text
}`, `INSERT INTO atlantis.urd_doc VALUES (1, 'kept')`)
	if _, err := conn.Exec(ctx, `ALTER TABLE atlantis.urd_doc DROP COLUMN title__parked`); err != nil {
		t.Fatalf("reap: %v", err)
	}

	v2 := lower(t, "entity Doc in urd {\n  id bigint primary\n  title text\n}")
	err := unparkExec(t, conn, ctx, without, v2, WithParkedFields(parked))
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("restore of a reaped column: %v; want the refusal", err)
	}
}

// A restored column declared against another table ends with that table's
// foreign key and not the parked one.
func TestARestoreRetargetsItsReference(t *testing.T) {
	conn, ctx := unparkConn(t)
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.urr_item, atlantis.urr_a, atlantis.urr_b CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'urr_item'`)
	}
	reset()
	t.Cleanup(reset)

	const targets = "entity A in urr { id bigint primary }\nentity B in urr { id bigint primary }\n"
	v1 := lower(t, targets+"entity Item in urr {\n  id bigint primary\n  owner_id bigint references urr.A.id\n}")
	without := lower(t, targets+"entity Item in urr { id bigint primary }")
	v2 := lower(t, targets+"entity Item in urr {\n  id bigint primary\n  owner_id bigint references urr.B.id\n}")

	initial, err := EmitInitial(v1)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up+`
INSERT INTO atlantis.urr_a VALUES (1); INSERT INTO atlantis.urr_b VALUES (1);
INSERT INTO atlantis.urr_item VALUES (1, 1);`); err != nil {
		t.Fatalf("initial: %v", err)
	}
	owner := WithCallerContext("backend", map[string]string{
		"urr.A": "backend", "urr.B": "backend", "urr.Item": "backend"}, nil)
	if err := unparkExec(t, conn, ctx, v1, without, owner); err != nil {
		t.Fatalf("park migration: %v", err)
	}
	parked := map[string]*dsl.Field{"urr.Item.owner_id": v1.Entities[2].FindField("owner_id")}
	if err := unparkExec(t, conn, ctx, without, v2, WithParkedFields(parked)); err != nil {
		t.Fatalf("restore with a new reference: %v", err)
	}

	var target string
	var fks int
	if err := conn.QueryRow(ctx, `
SELECT max(confrelid::regclass::text), count(*) FROM pg_constraint
 WHERE conrelid = 'atlantis.urr_item'::regclass AND contype = 'f'`).Scan(&target, &fks); err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	if fks != 1 || !strings.HasSuffix(target, "urr_b") {
		t.Errorf("%d foreign keys, referencing %s; want one, to urr_b", fks, target)
	}
}

// Both scripts restore the column as it was parked and then change it.
//
// In "widened", the restore through the backfill path is BACKFILL REQUIRED
// and the widening is ADDITIVE, a group that runs first; the widening needs
// the column the restore renames back.
//
// In "retyped", the restore's type guard passes only for the parked
// declaration's type, text, and the type change follows it.
func TestARestoreRunsBeforeItsOtherChanges(t *testing.T) {
	cases := []struct{ name, parked, declared string }{
		{"widened", "title varchar(10) not null", `title varchar(20) not null backfill "'filled'"`},
		{"retyped", "title text not null", `title varchar(20) not null backfill "'filled'"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, ctx := unparkConn(t)
			without, parked := parkDoc(t, conn, ctx,
				"entity Doc in urd {\n  id bigint primary\n  "+c.parked+"\n}",
				`INSERT INTO atlantis.urd_doc VALUES (1, 'kept')`)
			v2 := lower(t, "entity Doc in urd {\n  id bigint primary\n  "+c.declared+"\n}")
			scripts, err := EmitSQL(without, v2, ComputeDiff(without, v2, WithParkedFields(parked)))
			if err != nil {
				t.Fatalf("EmitSQL: %v", err)
			}
			for name, script := range map[string]string{"up": scripts.Up, "pre-backfill": scripts.PreBackfillUp} {
				tx, err := conn.Begin(ctx)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				if _, err := tx.Exec(ctx, script); err != nil {
					t.Errorf("%s: %v\n%s", name, err, script)
				}
				_ = tx.Rollback(ctx)
			}
		})
	}
}

// A field with a reference adds to an existing table, parks, and comes back
// with its constraint.
func TestAReferencedFieldAddsAndRestores(t *testing.T) {
	conn, ctx := unparkConn(t)
	reset := func() {
		_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.urf_item, atlantis.urf_owner CASCADE`)
		_, _ = conn.Exec(ctx, `DELETE FROM atlantis.parked_objects WHERE parent_table = 'urf_item'`)
	}
	reset()
	t.Cleanup(reset)

	schema := func(withRef bool) *dsl.IR {
		item := []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}
		if withRef {
			item = append(item, dsl.Field{Name: "owner_id", Type: dsl.FieldType{Name: "bigint"},
				Ref: &dsl.Ref{TargetID: "urf.Owner", TargetField: "id"}})
		}
		return &dsl.IR{Entities: []dsl.Entity{
			{Name: "Owner", Namespace: "urf", Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}},
			{Name: "Item", Namespace: "urf", Fields: item},
		}}
	}
	owner := WithCallerContext("backend",
		map[string]string{"urf.Owner": "backend", "urf.Item": "backend"}, nil)

	initial, err := EmitInitial(schema(false))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if _, err := conn.Exec(ctx, `
INSERT INTO atlantis.urf_owner VALUES (7);
INSERT INTO atlantis.urf_item VALUES (1);`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := unparkExec(t, conn, ctx, schema(false), schema(true), owner); err != nil {
		t.Fatalf("adding a referenced field: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE atlantis.urf_item SET owner_id = 7`); err != nil {
		t.Fatalf("set the reference: %v", err)
	}
	if err := unparkExec(t, conn, ctx, schema(true), schema(false), owner); err != nil {
		t.Fatalf("park migration: %v", err)
	}
	if err := unparkExec(t, conn, ctx, schema(false), schema(true), owner); err != nil {
		t.Fatalf("restore migration: %v", err)
	}

	var ownerID int64
	if err := conn.QueryRow(ctx, `SELECT owner_id FROM atlantis.urf_item WHERE id = 1`).Scan(&ownerID); err != nil {
		t.Fatalf("read the restored column: %v", err)
	}
	if ownerID != 7 {
		t.Errorf("restored owner_id = %d, want 7", ownerID)
	}
	var fks int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_constraint
 WHERE conrelid = 'atlantis.urf_item'::regclass AND contype = 'f'`).Scan(&fks); err != nil {
		t.Fatalf("count foreign keys: %v", err)
	}
	if fks != 1 {
		t.Errorf("the restored column has %d foreign keys, want 1", fks)
	}
}
