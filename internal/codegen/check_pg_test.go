package codegen

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Executes the generated DDL against a real PostgreSQL instead of asserting on
// its text.
//
// Every defect this file guards against was green under string assertions and
// only surfaced when the SQL ran: a duplicate constraint name aborting the
// migration, a DROP CONSTRAINT IF EXISTS naming something that never existed
// and therefore succeeding while changing nothing. "Contains ADD CONSTRAINT" is
// satisfied by SQL that fails, and by SQL that silently does nothing.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/codegen/ -run CheckDDLExecutes -v
func TestCheckDDLExecutesAgainstPostgres(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute generated CHECK DDL")
	}
	ctx := context.Background()

	base := func(checks []dsl.TableCheck, fieldCheck string, withTotal bool) *dsl.IR {
		fields := []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}}
		if withTotal {
			fields = append(fields, dsl.Field{
				Name: "total", Type: dsl.FieldType{Name: "int"}, Check: fieldCheck,
			})
		}
		return &dsl.IR{Entities: []dsl.Entity{{
			Name: "Order", Namespace: "ckt", Fields: fields, Checks: checks,
		}}}
	}

	for _, tc := range []struct {
		name string
		from *dsl.IR // nil => create the "from" state with EmitInitial
		to   *dsl.IR
		// want maps constraint name -> should it exist after the migration
		want map[string]bool
	}{
		{
			name: "add a named check",
			from: base(nil, "", true),
			to:   base([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "", true),
			want: map[string]bool{"total_positive": true},
		},
		{
			name: "remove a named check",
			from: base([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "", true),
			to:   base(nil, "", true),
			want: map[string]bool{"total_positive": false},
		},
		{
			name: "change a named predicate",
			from: base([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "", true),
			to:   base([]dsl.TableCheck{{Name: "total_positive", Expr: "total >= 0"}}, "", true),
			want: map[string]bool{"total_positive": true},
		},
		{
			name: "remove one of two checks sharing a predicate",
			from: base([]dsl.TableCheck{
				{Name: "a", Expr: "total > 0"},
				{Name: "b", Expr: "total > 0"},
			}, "", true),
			to:   base([]dsl.TableCheck{{Name: "a", Expr: "total > 0"}}, "", true),
			want: map[string]bool{"a": true, "b": false},
		},
		{
			// The case that aborted the migration: Postgres auto-names the
			// inline CHECK ckt_order_total_check, which is exactly what a
			// separate ADD CONSTRAINT would have used.
			name: "add a column carrying a check",
			from: base(nil, "", false),
			to:   base(nil, "total > 0", true),
			want: map[string]bool{"ckt_order_total_check": true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := pgx.Connect(ctx, url)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer conn.Close(ctx)

			// codegen puts every entity in the atlantis schema and folds the
			// namespace into the table name, so the fixture is one table there
			// rather than a schema of its own.
			resetFixture(ctx, t, url)

			initial, err := EmitInitial(tc.from)
			if err != nil {
				t.Fatalf("EmitInitial: %v", err)
			}
			if _, err := conn.Exec(ctx, initial.Up); err != nil {
				t.Fatalf("initial DDL failed:\n%s\n  %v", initial.Up, err)
			}

			migration, err := EmitSQL(tc.from, tc.to, ComputeDiff(tc.from, tc.to))
			if err != nil {
				t.Fatalf("EmitSQL: %v", err)
			}
			// The assertion that catches a duplicate-name collision: the
			// migration has to actually run.
			if _, err := conn.Exec(ctx, migration.Up); err != nil {
				t.Fatalf("migration failed:\n%s\n  %v", migration.Up, err)
			}

			for name, wantPresent := range tc.want {
				var present bool
				err := conn.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
   WHERE n.nspname = 'atlantis' AND t.relname = 'ckt_order'
     AND c.conname = $1 AND c.contype = 'c')`,
					name).Scan(&present)
				if err != nil {
					t.Fatalf("query pg_constraint: %v", err)
				}
				if present != wantPresent {
					t.Errorf("constraint %q present=%v, want %v.\n"+
						"  The migration reported success either way — this is the "+
						"assertion that distinguishes a DROP that worked from a "+
						"DROP CONSTRAINT IF EXISTS that named the wrong thing.\n%s",
						name, present, wantPresent, migration.Up)
				}
			}

			// The down-migration must also run. An inverted statement order
			// (ADD CONSTRAINT before the column it references is recreated)
			// only shows up here.
			if _, err := conn.Exec(ctx, migration.Down); err != nil {
				t.Errorf("down-migration failed, so the rollback path is unrunnable:\n%s\n  %v",
					migration.Down, err)
			}
		})
	}

	t.Run("predicate is actually enforced after the migration", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)

		resetFixture(ctx, t, url)

		from := base(nil, "", true)
		to := base([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "", true)

		initial, _ := EmitInitial(from)
		if _, err := conn.Exec(ctx, initial.Up); err != nil {
			t.Fatalf("initial: %v", err)
		}
		mig, _ := EmitSQL(from, to, ComputeDiff(from, to))
		if _, err := conn.Exec(ctx, mig.Up); err != nil {
			t.Fatalf("migration: %v", err)
		}

		// Presence in pg_constraint is not enforcement. A constraint added
		// NOT VALID would be present and would accept this row.
		_, err = conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (1, -5)`)
		if err == nil {
			t.Error("a row violating the new CHECK was accepted — the constraint is " +
				"present but not enforcing")
		}

		// And the migration must have removed nothing it should not have: a
		// legal row still inserts.
		if _, err := conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (2, 5)`); err != nil {
			t.Errorf("a legal row was rejected: %v", err)
		}
	})

	// Data that violates a proposed CHECK must make the migration fail rather
	// than silently skip validation. This is the behaviour rehearsal probes are
	// meant to predict, so it is worth pinning that it is real.
	t.Run("existing violating rows fail the migration", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)

		resetFixture(ctx, t, url)

		from := base(nil, "", true)
		to := base([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "", true)

		initial, _ := EmitInitial(from)
		if _, err := conn.Exec(ctx, initial.Up); err != nil {
			t.Fatalf("initial: %v", err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (1, -5)`); err != nil {
			t.Fatalf("seed: %v", err)
		}

		mig, _ := EmitSQL(from, to, ComputeDiff(from, to))
		if _, err := conn.Exec(ctx, mig.Up); err == nil {
			t.Error("adding a CHECK over a violating row succeeded — either the " +
				"constraint was not really added, or it was added NOT VALID")
		} else {
			t.Logf("correctly refused: %v", err)
		}
	})
}

// resetFixture drops the fixture table before and after a subtest. Dropping
// rather than truncating matters: a leftover constraint from an earlier case
// would make a later ADD CONSTRAINT collide and the failure would read as a
// codegen bug.
func resetFixture(ctx context.Context, t *testing.T, url string) {
	t.Helper()
	drop := func() {
		c, err := pgx.Connect(context.Background(), url)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.ckt_order CASCADE`)
	}
	drop()
	t.Cleanup(drop)
}

// After a predicate change, the NEW predicate must be what the database
// enforces.
//
// Nothing asserted this. Mutating emitCheckAdd's up-path to re-apply ch.From —
// so the migration reverts the predicate instead of changing it — left the
// entire repository green. The existing tests count the constraint *name* twice
// and check the constraint still exists afterwards, both of which are true of a
// migration that changed nothing.
func TestChangedPredicateEnforcesTheNewOne(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute generated CHECK DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	resetFixture(ctx, t, url)

	ent := func(expr string) *dsl.IR {
		return &dsl.IR{Entities: []dsl.Entity{{
			Name: "Order", Namespace: "ckt",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "total", Type: dsl.FieldType{Name: "int"}},
			},
			Checks: []dsl.TableCheck{{Name: "total_ok", Expr: expr}},
		}}}
	}
	from := ent("total > 10")
	to := ent("total > 0")

	initial, err := EmitInitial(from)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}
	mig, err := EmitSQL(from, to, ComputeDiff(from, to))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("migration: %v\n%s", err, mig.Up)
	}

	// total = 5 satisfies the new predicate and violates the old one. It is the
	// single row that distinguishes "changed" from "left alone".
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (1, 5)`); err != nil {
		t.Errorf("total=5 was rejected, so the OLD predicate (total > 10) is still "+
			"enforced — the migration did not change anything:\n%s\n  %v", mig.Up, err)
	}
	// And the new predicate must genuinely be in force, not simply absent.
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (2, -1)`); err == nil {
		t.Error("total=-1 was accepted, so no predicate is enforced — the migration " +
			"dropped the constraint without re-adding it")
	}
}

// Rolling back a migration that dropped a column plus the entity-level CHECK
// referencing it.
//
// The up script drops the constraint (ADDITIVE) then the column (BREAKING). The
// down script used to emit its groups in the same order, so it re-added the
// constraint before the column existed:
//
//	ERROR:  column "total" does not exist
//
// Every table-driven case above runs migration.Down, but none of them needed
// down to restore a column, so none could see it.
func TestRollbackRestoresColumnBeforeItsCheck(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute generated CHECK DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	resetFixture(ctx, t, url)

	with := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Order", Namespace: "ckt",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "total", Type: dsl.FieldType{Name: "int"}},
		},
		Checks: []dsl.TableCheck{{Name: "total_pos", Expr: "total > 0"}},
	}}}
	without := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Order", Namespace: "ckt",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}},
	}}}

	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}
	mig, err := EmitSQL(with, without, ComputeDiff(with, without))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("up: %v\n%s", err, mig.Up)
	}
	if _, err := conn.Exec(ctx, mig.Down); err != nil {
		t.Fatalf("down is unrunnable, so this migration cannot be rolled back:\n%s\n  %v",
			mig.Down, err)
	}

	// And the rollback must have restored the constraint, not just the column.
	if _, err := conn.Exec(ctx, `INSERT INTO atlantis.ckt_order (id, total) VALUES (1, -5)`); err == nil {
		t.Error("after rollback a row violating the restored CHECK was accepted")
	}
}

// Long identifiers. Postgres silently truncates an over-long constraint name to
// 63 bytes, so a CREATE that stores a truncated name and a DROP that supplies
// the full one refer to different objects — and DROP CONSTRAINT IF EXISTS turns
// that mismatch into a reported success with the constraint still enforcing.
//
// Confirmed before the fix: a 48-byte table and a 25-byte column produced an
// 80-byte name, Postgres stored 63 different bytes, and the removal migration
// left the constraint in place.
func TestLongConstraintNamesRoundTrip(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute generated CHECK DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	const table = "zzlongns_an_entity_with_a_really_quite_long_name"
	drop := func() {
		c, err := pgx.Connect(context.Background(), url)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.`+table+` CASCADE`)
	}
	drop()
	t.Cleanup(drop)

	col := "a_column_with_a_long_name"
	mk := func(check string) *dsl.IR {
		return &dsl.IR{Entities: []dsl.Entity{{
			Name: "AnEntityWithAReallyQuiteLongName", Namespace: "zzlongns",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: col, Type: dsl.FieldType{Name: "int"}, Check: check},
			},
		}}}
	}
	with, without := mk(col+" > 0"), mk("")

	initial, err := EmitInitial(with)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("initial: %v\n%s", err, initial.Up)
	}

	// The name we emitted must be the name Postgres stored. Any divergence here
	// is the whole defect: everything downstream targets our name.
	var stored string
	if err := conn.QueryRow(ctx, `
SELECT c.conname FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = $1 AND c.contype = 'c'`, table).Scan(&stored); err != nil {
		t.Fatalf("no CHECK constraint was created: %v", err)
	}
	if len(stored) > 63 {
		t.Fatalf("stored name is %d bytes, which Postgres cannot hold", len(stored))
	}

	mig, err := EmitSQL(with, without, ComputeDiff(with, without))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := conn.Exec(ctx, mig.Up); err != nil {
		t.Fatalf("removal migration: %v\n%s", err, mig.Up)
	}

	var still bool
	if err := conn.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = $1 AND c.contype = 'c')`, table).Scan(&still); err != nil {
		t.Fatalf("query: %v", err)
	}
	if still {
		t.Errorf("the constraint survived its own removal migration — the DROP named "+
			"%q, Postgres stored something else, and IF EXISTS reported success:\n%s",
			stored, mig.Up)
	}
}

// An author-supplied entity check name equal to a field check's generated name.
//
// resolveCheckNames renames the field check out of the way, but that rename was
// fiction: columnDecl inlined the check unnamed and Postgres auto-named it back
// onto the colliding name, so EmitInitial produced DDL that could not apply.
func TestAuthorNameCollidingWithAFieldCheckStillApplies(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute generated CHECK DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	resetFixture(ctx, t, url)

	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Order", Namespace: "ckt",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "total", Type: dsl.FieldType{Name: "int"}, Check: "total > 0"},
		},
		// Deliberately the name the field check would otherwise take.
		Checks: []dsl.TableCheck{{Name: "ckt_order_total_check", Expr: "total < 100"}},
	}}}

	initial, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := conn.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("DDL does not apply — the rename resolveCheckNames performed was "+
			"never written:\n%s\n  %v", initial.Up, err)
	}

	var n int
	if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = 'ckt_order' AND c.contype = 'c'`).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	if n != 2 {
		t.Errorf("got %d CHECK constraints, want 2 — both predicates must survive "+
			"the rename:\n%s", n, initial.Up)
	}
}
