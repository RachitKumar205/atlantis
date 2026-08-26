package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Emitter-level regressions. Each of these was confirmed against a live
// PostgreSQL 17 before the fix: the generated migration either aborted or
// silently did nothing. They assert on the constraint NAME, which is where
// every one of them lived — the two earlier EmitSQL tests only checked that the
// string "ADD CONSTRAINT" appeared, and stayed green through all of it.

func vIR(e dsl.Entity) *dsl.IR { return &dsl.IR{Entities: []dsl.Entity{e}} }

func emitUp(t *testing.T, old, nw *dsl.IR) string {
	t.Helper()
	s, err := EmitSQL(old, nw, ComputeDiff(old, nw))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	return s.Up
}

// Removing a named entity-level check must DROP that name.
//
// checkConstraintName is handed the new entity, which no longer holds the
// removed constraint. Scanning that and falling back to a hash of the predicate
// names a constraint that never existed, and IF EXISTS turns the DROP into a
// no-op: the plan reports the constraint gone while the database enforces it.
func TestRemovingANamedCheckDropsThatName(t *testing.T) {
	with := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}},
		Checks: []dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}}
	without := with
	without.Checks = nil
	up := emitUp(t, vIR(with), vIR(without))
	if !strings.Contains(up, `"total_positive"`) {
		t.Errorf("DROP does not name total_positive")
	}
}

// Two named checks may share a predicate. Removing one must drop that one.
//
// Keyed by predicate, the emitter's scan resolves the survivor's name and
// emits DROP CONSTRAINT "a" for a removal of "b": the kept constraint is
// destroyed, the removed one stays live, and the plan reports neither.
func TestRemovingOneOfTwoChecksSharingAPredicateDropsTheRightOne(t *testing.T) {
	two := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}},
		Checks: []dsl.TableCheck{{Name: "a", Expr: "total > 0"}, {Name: "b", Expr: "total > 0"}}}
	one := two
	one.Checks = []dsl.TableCheck{{Name: "a", Expr: "total > 0"}}
	up := emitUp(t, vIR(two), vIR(one))
	if !strings.Contains(up, `"b"`) || strings.Contains(up, `"a"`) {
		t.Errorf("wrong constraint dropped")
	}
}

// A changed predicate must drop and add under the same name.
//
// The drop was built from a synthetic Change that omitted the resolved name, so
// it hashed the old predicate, dropped nothing, and the following ADD collided
// with the constraint still in place:
//
//	ERROR:  constraint "total_positive" for relation "shop_order" already exists
func TestChangedPredicateDropsAndAddsTheSameName(t *testing.T) {
	before := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}},
		Checks: []dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}}
	after := before
	after.Checks = []dsl.TableCheck{{Name: "total_positive", Expr: "total >= 0"}}
	up := emitUp(t, vIR(before), vIR(after))
	if strings.Count(up, `"total_positive"`) != 2 {
		t.Errorf("drop and add do not both target total_positive")
	}
}

// Two identical unnamed predicates are two constraints in the catalog.
func TestDeletingOneOfTwoIdenticalUnnamedChecksIsVisible(t *testing.T) {
	two := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}},
		Checks: []dsl.TableCheck{{Expr: "total > 0"}, {Expr: "total > 0"}}}
	one := two
	one.Checks = []dsl.TableCheck{{Expr: "total > 0"}}
	d := ComputeDiff(vIR(two), vIR(one))
	n := len(d.Additive) + len(d.BackfillRequired) + len(d.Breaking)
	if n != 1 {
		t.Errorf("got %d changes, want 1", n)
	}
}

// Fresh-create and migrated must agree on the name.
func TestFreshCreateAndMigrationAgreeOnTheName(t *testing.T) {
	base := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}}}
	withChk := base
	withChk.Checks = []dsl.TableCheck{{Expr: "total > 0"}}

	fresh, err := EmitInitial(vIR(withChk))
	if err != nil {
		t.Fatal(err)
	}
	migrated := emitUp(t, vIR(base), vIR(withChk))

	name := unnamedCheckName(&withChk, "total > 0", 1)
	if !strings.Contains(fresh.Up, name) {
		t.Errorf("EmitInitial does not use %s:\n%s", name, fresh.Up)
	}
	if !strings.Contains(migrated, name) {
		t.Errorf("migration does not use %s:\n%s", name, migrated)
	}
}

// diffEntity calls every sub-differ, so before readsField stopped at claimed
// differs, {diffed, "diffEntity"} satisfied every attribute any of them read —
// one plausible name blanket-covering the registry.
func TestDiffEntityCannotLaunderCoverage(t *testing.T) {
	all := differFuncsInPackage(t)
	claimed := claimedDiffers()
	for _, f := range []string{"Kind", "Checks", "Indexes", "Uniques"} {
		if readsField(all, "diffEntity", f, claimed) {
			t.Errorf("diffEntity launders coverage for %s", f)
		}
	}
	if !readsField(all, "diffChecks", "Checks", claimed) {
		t.Error("regression: diffChecks no longer covers Checks")
	}
	if !readsField(all, "diffIndexes", "Kind", claimed) {
		t.Error("regression: diffIndexes no longer reads Kind")
	}
}

// Duplicate author-supplied names must fail loudly.
func TestDuplicateConstraintNamesAreRejected(t *testing.T) {
	e := dsl.Entity{Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true}, {Name: "total", Type: dsl.FieldType{Name: "int"}}},
		Checks: []dsl.TableCheck{{Name: "dup", Expr: "total > 0"}, {Name: "dup", Expr: "id > 0"}}}
	if _, err := EmitInitial(vIR(e)); err == nil {
		t.Error("duplicate constraint names emitted without error")
	} else {
	}
}

// The duplicate-name guard has to run on both IRs in EmitSQL, not only in
// EmitInitial.
//
// Deleting assertCheckNamesUnique from EmitSQL left the whole suite green,
// because the only test covering it went through EmitInitial. And the old-IR
// half matters on its own: the down script re-creates entities dropped by this
// migration from the old IR, so a duplicate there produces a rollback Postgres
// rejects while EmitInitial would have refused the identical schema.
func TestEmitSQLRejectsDuplicateNamesOnBothSides(t *testing.T) {
	clean := dsl.Entity{Name: "Order", Namespace: "shop", Fields: []dsl.Field{
		{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
		{Name: "total", Type: dsl.FieldType{Name: "int"}},
	}}
	dup := clean
	dup.Checks = []dsl.TableCheck{
		{Name: "dup", Expr: "total > 0"},
		{Name: "dup", Expr: "id > 0"},
	}

	t.Run("new side", func(t *testing.T) {
		a, b := vIR(clean), vIR(dup)
		if _, err := EmitSQL(a, b, ComputeDiff(a, b)); err == nil {
			t.Error("EmitSQL emitted a schema with two constraints named \"dup\"; " +
				"Postgres rejects it, so the migration cannot apply")
		}
	})

	t.Run("old side", func(t *testing.T) {
		// The entity is dropped, so the down script re-creates it from the old
		// IR — carrying the duplicate with it.
		a, b := vIR(dup), &dsl.IR{}
		if _, err := EmitSQL(a, b, ComputeDiff(a, b)); err == nil {
			t.Error("EmitSQL accepted a prior schema with duplicate constraint names; " +
				"the generated down-migration re-creates the entity from it and " +
				"Postgres will reject the rollback")
		}
	})
}
