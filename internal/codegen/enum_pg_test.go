package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The emitted CREATE TYPE runs, the column is of that type, a label
// round-trips as a Go string, and Postgres refuses a label the type does not
// list.
//
// The last of those is why an enum column needs no validation of its own: the
// constraint is the type, and the database enforces it.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/codegen -run Enum -count=1
func TestEnumAppliesToPostgres(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the emitted DDL against a real database")
	}

	f, err := dsl.Parse("t.atl", []byte(enumSchema))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("Postgres refused the emitted DDL: %v\n%s", err, scripts.Up)
	}

	// The column is the enum type, in the schema atlantis owns.
	//
	// Not asserted through format_type: it qualifies a type only when the type
	// is not visible on the search_path, so the same column reads back as
	// "app_mood" or "atlantis.app_mood" depending on the connection. Both
	// sides of a drift comparison see the same rendering, which is what makes
	// that safe there and useless here.
	var typname, typns, typtype string
	err = tx.QueryRow(ctx, `
		SELECT ty.typname, n.nspname, ty.typtype
		  FROM pg_attribute a
		  JOIN pg_class c ON c.oid = a.attrelid
		  JOIN pg_type ty ON ty.oid = a.atttypid
		  JOIN pg_namespace n ON n.oid = ty.typnamespace
		 WHERE c.relname = 'app_person' AND a.attname = 'mood'`).Scan(&typname, &typns, &typtype)
	if err != nil {
		t.Fatalf("read the column: %v", err)
	}
	if typname != "app_mood" {
		t.Errorf("mood's type is %q, want %q", typname, "app_mood")
	}
	if typns != "atlantis" {
		t.Errorf("mood's type lives in %q, want atlantis", typns)
	}
	if typtype != "e" {
		t.Errorf("mood's type reports typtype %q, want \"e\"", typtype)
	}

	// The labels, in declaration order, which is the order Postgres sorts in.
	rows, err := tx.Query(ctx, `
		SELECT e.enumlabel
		  FROM pg_enum e
		  JOIN pg_type t ON t.oid = e.enumtypid
		 WHERE t.typname = 'app_mood'
		 ORDER BY e.enumsortorder`)
	if err != nil {
		t.Fatalf("read labels: %v", err)
	}
	var labels []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		labels = append(labels, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"happy", "sad", "in progress"}
	if len(labels) != len(want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label %d = %q, want %q", i, labels[i], want[i])
		}
	}

	// A label binds and reads back as a Go string, with no codec and no cast.
	if _, err := tx.Exec(ctx,
		`INSERT INTO atlantis.app_person (id, mood, fallback) VALUES ($1, $2, $3)`,
		1, "in progress", "happy"); err != nil {
		t.Fatalf("bind a label: %v", err)
	}
	var got string
	if err := tx.QueryRow(ctx, `SELECT mood FROM atlantis.app_person WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("scan a label: %v", err)
	}
	if got != "in progress" {
		t.Errorf("read back %q, want %q", got, "in progress")
	}

	// A label the type does not list is refused by Postgres, which is why the
	// declaration carries no check of its own. In a savepoint: the failure
	// aborts the transaction otherwise.
	if _, err := tx.Exec(ctx, `SAVEPOINT bad`); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO atlantis.app_person (id, fallback) VALUES ($1, $2)`, 2, "nope")
	if err == nil {
		t.Error("Postgres accepted a label the enum does not list")
	}
	_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT bad`)
}

// Adding a label produces DDL that runs, and the constraint the refusal is
// based on is real.
//
// The two migrations run in separate transactions, which is how apply runs
// them. Postgres exempts a type created in the same transaction from the
// use-after-add rule, so running both in one transaction tests the exemption
// rather than the rule — and reports the refusal as unnecessary.
func TestEnumValueAddedAppliesToPostgres(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the emitted DDL against a real database")
	}

	lower := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("t.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	entity := "entity Person in enumcommit {\n  id bigint primary\n  mood Mood not null\n}\n"
	oldIR := lower("enum Mood in enumcommit { happy, sad }\n" + entity)
	newIR := lower("enum Mood in enumcommit { happy, sad, neutral }\n" + entity)

	initial, err := EmitInitial(oldIR)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	migration, err := EmitSQL(oldIR, newIR, ComputeDiff(oldIR, newIR))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if !strings.Contains(migration.Up, "ADD VALUE IF NOT EXISTS 'neutral'") {
		t.Fatalf("the migration does not add the label:\n%s", migration.Up)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Named so the cleanup drops exactly what this test made, since the
	// objects live in the shared atlantis schema.
	// Its own connection: t.Cleanup runs after the deferred Close above, so a
	// cleanup on `conn` executes against a closed connection and the objects
	// survive into the next run.
	t.Cleanup(func() {
		c := context.Background()
		cc, err := pgx.Connect(c, dsn)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer func() { _ = cc.Close(c) }()
		if _, err := cc.Exec(c, `DROP TABLE IF EXISTS atlantis.enumcommit_person`); err != nil {
			t.Errorf("cleanup table: %v", err)
		}
		if _, err := cc.Exec(c, `DROP TYPE IF EXISTS atlantis.enumcommit_mood`); err != nil {
			t.Errorf("cleanup type: %v", err)
		}
	})
	_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.enumcommit_person`)
	_, _ = conn.Exec(ctx, `DROP TYPE IF EXISTS atlantis.enumcommit_mood`)

	// Transaction one: the type and the table.
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial DDL: %v", err)
	}

	// Transaction two: the migration, as apply runs it.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, migration.Up); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Postgres refused the migration: %v\n%s", err, migration.Up)
	}

	// The rule refuseLabelUsedWhenAdded is based on. If Postgres ever allows
	// this, that refusal costs a legal migration and should go.
	_, err = tx.Exec(ctx, `INSERT INTO atlantis.enumcommit_person (id, mood) VALUES (1, 'neutral')`)
	if err == nil {
		t.Error("Postgres used a label added in the same transaction; " +
			"refuseLabelUsedWhenAdded is refusing something legal")
	} else if !strings.Contains(err.Error(), "unsafe use of new value") {
		t.Errorf("refused, but not for the reason the refusal claims: %v", err)
	}
	_ = tx.Rollback(ctx)

	// Committed on its own, the label is usable.
	if _, err := conn.Exec(ctx, migration.Up); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.enumcommit_person (id, mood) VALUES (2, 'neutral')`); err != nil {
		t.Errorf("the label is unusable after its migration committed: %v", err)
	}
}

// A label removed has no DDL Postgres can run, so the migration is refused
// rather than emitted.
func TestEnumValueRemovedIsRefused(t *testing.T) {
	lower := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("t.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	entity := "entity Person in app {\n  id bigint primary\n  mood Mood not null\n}\n"
	oldIR := lower("enum Mood in app { happy, sad }\n" + entity)
	newIR := lower("enum Mood in app { happy }\n" + entity)

	_, err := EmitSQL(oldIR, newIR, ComputeDiff(oldIR, newIR))
	if err == nil {
		t.Fatal("a removed label emitted DDL; Postgres has no ALTER TYPE DROP VALUE")
	}
	if !strings.Contains(err.Error(), "DROP VALUE") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// Adding a label and defaulting a column to it in one migration is refused
// with the reason, rather than failing at the database.
func TestALabelCannotBeAddedAndUsedTogether(t *testing.T) {
	lower := func(src string) *dsl.IR {
		t.Helper()
		f, err := dsl.Parse("t.atl", []byte(src))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		ir, err := dsl.Lower([]*dsl.File{f})
		if err != nil {
			t.Fatalf("lower: %v", err)
		}
		return ir
	}
	oldIR := lower("enum Mood in app { happy }\n" +
		"entity Person in app {\n  id bigint primary\n  mood Mood not null\n}\n")
	newIR := lower("enum Mood in app { happy, neutral }\n" +
		"entity Person in app {\n  id bigint primary\n  mood Mood not null default \"neutral\"\n}\n")

	_, err := EmitSQL(oldIR, newIR, ComputeDiff(oldIR, newIR))
	if err == nil {
		t.Fatal("emitted a migration Postgres refuses as `unsafe use of new value`")
	}
	if !strings.Contains(err.Error(), "transaction that") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}
