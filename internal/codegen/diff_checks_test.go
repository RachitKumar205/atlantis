package codegen

import (
	"strconv"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func entityWithChecks(checks []dsl.TableCheck, fieldCheck string) *dsl.Entity {
	return &dsl.Entity{
		Name: "Order", Namespace: "shop",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "total", Type: dsl.FieldType{Name: "int"}, Check: fieldCheck},
		},
		Checks: checks,
	}
}

// The headline case. Before diffChecks existed, adding a CHECK produced an
// empty diff — so the emitted DDL contained no CHECK, and a rehearsal probe
// examining that DDL would have reported pass for a constraint that was never
// going to be validated.
func TestAddingACheckIsDetected(t *testing.T) {
	oldE := entityWithChecks(nil, "")
	newE := entityWithChecks([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 {
		t.Fatalf("expected one change, got %d: %+v", len(got), got)
	}
	if got[0].Kind != KindCheckAdded {
		t.Errorf("Kind = %v, want %v", got[0].Kind, KindCheckAdded)
	}
	// Adding a CHECK validates every existing row under an exclusive lock, so
	// it is a data-dependent operation, not an additive one.
	if got[0].Class != ClassBackfillRequired {
		t.Errorf("Class = %v, want %v — adding a CHECK can fail on existing data",
			got[0].Class, ClassBackfillRequired)
	}
}

// Dropping cannot fail on data and takes only a brief lock: rows the constraint
// rejected become legal.
func TestRemovingACheckIsAdditive(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "")
	newE := entityWithChecks(nil, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 || got[0].Kind != KindCheckRemoved {
		t.Fatalf("expected one check_removed, got %+v", got)
	}
	if got[0].Class != ClassAdditive {
		t.Errorf("Class = %v, want additive", got[0].Class)
	}
}

// A changed predicate is one change, not remove+add: the DDL must drop and
// re-add in order, and splitting it risks a reordering that leaves the table
// briefly unconstrained.
func TestChangingAPredicateIsOneChange(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, "")
	newE := entityWithChecks([]dsl.TableCheck{{Name: "total_positive", Expr: "total >= 0"}}, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 {
		t.Fatalf("expected one change, got %d: %+v", len(got), got)
	}
	if got[0].Kind != KindCheckChanged {
		t.Errorf("Kind = %v, want %v", got[0].Kind, KindCheckChanged)
	}
	if got[0].From != "total > 0" || got[0].To != "total >= 0" {
		t.Errorf("From/To = %v/%v, want the old and new predicates", got[0].From, got[0].To)
	}
}

// Per-field `check` modifiers are the same hazard through a different syntax,
// and were equally invisible.
func TestFieldLevelCheckIsDetected(t *testing.T) {
	oldE := entityWithChecks(nil, "")
	newE := entityWithChecks(nil, "total > 0")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 || got[0].Kind != KindCheckAdded {
		t.Fatalf("a per-field check modifier was not detected: %+v", got)
	}
}

// Unnamed checks must be identified by predicate, not by position.
//
// Comparing two identical entities and asserting an empty diff separates
// nothing: that holds for any deterministic keying, positional included.
//
// Under positional names, deleting the first of two unnamed checks renumbers
// the second, so a pure removal reports as a removal plus a predicate change on
// an untouched constraint. That escalates the plan to backfill-required, which
// exits 1 in caller CI.
func TestDeletingOneUnnamedCheckIsOneRemoval(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{{Expr: "total > 0"}, {Expr: "id > 0"}}, "")
	newE := entityWithChecks([]dsl.TableCheck{{Expr: "id > 0"}}, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 {
		t.Fatalf("deleting one of two unnamed checks must be exactly one change, got %d: %+v",
			len(got), got)
	}
	if got[0].Kind != KindCheckRemoved {
		t.Errorf("Kind = %v, want %v — the surviving check was reported as modified",
			got[0].Kind, KindCheckRemoved)
	}
	if got[0].From != "total > 0" {
		t.Errorf("removed the wrong predicate: %v", got[0].From)
	}
}

// A user who copies a constraint name out of the live database can land on the
// emitter's own generated form for an unnamed check. If that collides, one
// CHECK vanishes from the diff in both directions — the exact invisibility this
// differ exists to remove. Derived rather than hardcoded, so it keeps testing
// the collision if the naming scheme changes again.
func TestNamedCheckCannotClobberAnUnnamedOne(t *testing.T) {
	probe := entityWithChecks([]dsl.TableCheck{{Expr: "b > 0"}}, "")
	generated := unnamedCheckName(probe, "b > 0", 1)

	e := entityWithChecks([]dsl.TableCheck{
		{Name: generated, Expr: "a > 0"},
		{Expr: "b > 0"},
	}, "")
	if got := len(resolvedChecks(e)); got != 2 {
		t.Fatalf("a user-supplied name equal to the generated one (%q) collapsed "+
			"2 checks into %d — one is invisible to the differ", generated, got)
	}
}

// Two named checks may share a predicate; they are still two constraints.
//
// Keyed by predicate they collapse to one entry, so removing one produces no
// change, and the emitter's scan resolves the survivor's name — DROP CONSTRAINT
// "a" for a removal of "b". The kept constraint is destroyed, the removed one
// stays live, and the plan reports neither.
func TestTwoNamedChecksSharingAPredicateAreDistinct(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{
		{Name: "a", Expr: "total > 0"},
		{Name: "b", Expr: "total > 0"},
	}, "")
	newE := entityWithChecks([]dsl.TableCheck{{Name: "a", Expr: "total > 0"}}, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 {
		t.Fatalf("removing b produced %d changes, want 1: %+v", len(got), got)
	}
	if got[0].Constraint != "b" {
		t.Errorf("Constraint = %q, want \"b\" — the emitter would drop the wrong "+
			"constraint, destroying one the author kept", got[0].Constraint)
	}
}

// Two identical unnamed predicates are two constraints in the catalog, and
// deleting one must be visible. Keyed by predicate alone they collapsed to a
// single entry, so the removal produced no change and no DDL.
func TestTwoIdenticalUnnamedChecksAreDistinct(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{{Expr: "total > 0"}, {Expr: "total > 0"}}, "")
	newE := entityWithChecks([]dsl.TableCheck{{Expr: "total > 0"}}, "")

	if got := len(resolvedChecks(oldE)); got != 2 {
		t.Fatalf("two identical unnamed checks resolved to %d entries, want 2", got)
	}
	d := &Diff{}
	diffChecks(oldE, newE, d)
	if got := allChanges(d); len(got) != 1 {
		t.Fatalf("deleting one of two identical unnamed checks produced %d changes, "+
			"want 1: %+v", len(got), got)
	}
}

// Reordering unnamed checks must be a no-op. Positional naming made it a
// rename, which the differ then reported as a predicate change plus a removal —
// an ACCESS EXCLUSIVE revalidation of the whole table for an unchanged schema.
func TestReorderingUnnamedChecksIsNotAChange(t *testing.T) {
	oldE := entityWithChecks([]dsl.TableCheck{{Expr: "total > 0"}, {Expr: "id > 0"}}, "")
	newE := entityWithChecks([]dsl.TableCheck{{Expr: "id > 0"}, {Expr: "total > 0"}}, "")

	d := &Diff{}
	diffChecks(oldE, newE, d)
	if got := allChanges(d); len(got) != 0 {
		t.Errorf("reordering produced %d changes, want 0: %+v", len(got), got)
	}
}

// A check on a column that is itself new travels with the column: columnDecl
// inlines it into ADD COLUMN, and Postgres auto-names that inline constraint
// <table>_<column>_check. Emitting a separate ADD CONSTRAINT under the same
// name aborts the migration:
//
//	ERROR:  constraint "shop_order_total_check" for relation "shop_order" already exists
//
// A check written directly after a field is absorbed as that field's modifier
// (task #31), so this form is what most .atl files produce today.
func TestCheckOnANewColumnDoesNotDoubleEmit(t *testing.T) {
	oldE := &dsl.Entity{Name: "Order", Namespace: "shop", Fields: []dsl.Field{
		{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
	}}
	newE := entityWithChecks(nil, "total > 0")

	d := &Diff{}
	diffChecks(oldE, newE, d)
	if got := allChanges(d); len(got) != 0 {
		t.Fatalf("a check on a brand-new column produced %d separate change(s): %+v\n"+
			"  The constraint ships inline with ADD COLUMN; a second ADD CONSTRAINT "+
			"collides with it by name and fails the migration.", len(got), got)
	}

	// End to end, so this is proven about the DDL rather than the diff.
	oldIR := &dsl.IR{Entities: []dsl.Entity{*oldE}}
	newIR := &dsl.IR{Entities: []dsl.Entity{*newE}}
	scripts, err := EmitSQL(oldIR, newIR, ComputeDiff(oldIR, newIR))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if n := strings.Count(scripts.Up, "total > 0"); n != 1 {
		t.Errorf("predicate appears %d times in the up-migration, want 1:\n%s", n, scripts.Up)
	}
	if strings.Contains(scripts.Up, "ADD CONSTRAINT") {
		t.Errorf("emitted a separate ADD CONSTRAINT for a check that ships with "+
			"ADD COLUMN:\n%s", scripts.Up)
	}
}

// Dropping a column takes its check with it; a separate DROP CONSTRAINT is
// redundant and, on the down-migration, is ordered before the column exists.
func TestCheckOnADroppedColumnDoesNotDoubleEmit(t *testing.T) {
	oldE := entityWithChecks(nil, "total > 0")
	newE := &dsl.Entity{Name: "Order", Namespace: "shop", Fields: []dsl.Field{
		{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
	}}

	d := &Diff{}
	diffChecks(oldE, newE, d)
	if got := allChanges(d); len(got) != 0 {
		t.Errorf("dropping a column with a check produced %d separate check change(s): %+v",
			len(got), got)
	}
}

// Per-field checks must carry the column, or no emitter can name the constraint
// and every downstream consumer renders a blank field.
func TestFieldLevelCheckCarriesTheColumn(t *testing.T) {
	oldE := entityWithChecks(nil, "")
	newE := entityWithChecks(nil, "total > 0")

	d := &Diff{}
	diffChecks(oldE, newE, d)

	got := allChanges(d)
	if len(got) != 1 {
		t.Fatalf("expected one change, got %+v", got)
	}
	if got[0].Field != "total" {
		t.Errorf("Field = %q, want \"total\"", got[0].Field)
	}
}

func allChanges(d *Diff) []Change {
	var out []Change
	out = append(out, d.Additive...)
	out = append(out, d.BackfillRequired...)
	out = append(out, d.Breaking...)
	return out
}

// TestComputeDiffSurfacesCheckChanges goes through the real entry point rather
// than calling diffChecks directly.
//
// The unit tests above prove the differ works; they do not prove anything calls
// it. Verified by mutation: deleting the diffChecks line from diffEntity left
// every one of them green. That is the same defect pattern as a validator with
// no call site — the function is correct and unreachable, which is
// indistinguishable from absent.
func TestComputeDiffSurfacesCheckChanges(t *testing.T) {
	oldIR := &dsl.IR{Entities: []dsl.Entity{*entityWithChecks(nil, "")}}
	newIR := &dsl.IR{Entities: []dsl.Entity{
		*entityWithChecks([]dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}}, ""),
	}}

	d := ComputeDiff(oldIR, newIR)

	var found *Change
	for _, c := range allChanges(d) {
		if c.Kind == KindCheckAdded {
			found = &c
			break
		}
	}
	if found == nil {
		t.Fatalf("ComputeDiff did not surface an added CHECK — diffEntity is not calling "+
			"diffChecks, so the emitted DDL will contain no CHECK and rehearsal would "+
			"report pass for a constraint it never examined. Changes: %+v", allChanges(d))
	}
	if found.Class != ClassBackfillRequired {
		t.Errorf("Class = %v, want backfill_required", found.Class)
	}
}

// Round-trip: what up creates, down must remove — under the same name.
//
// Asserting only that "ADD CONSTRAINT" appears in the up-migration stays green
// through five separate defects that each emit SQL which aborts or silently
// does nothing: every one of them is in the constraint name, and that assertion
// reads no name. It is satisfied by SQL that fails.
//
// check_emit_test.go asserts on names; check_pg_test.go executes the DDL. This
// keeps the pairing property the other two do not cover: the identifier down
// drops is the identifier up created.
func TestUpAndDownAgreeOnTheConstraintName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, to   []dsl.TableCheck
		fieldFrom  string
		fieldTo    string
		constraint string
	}{
		{
			name:       "named entity check added",
			to:         []dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}},
			constraint: "total_positive",
		},
		{
			name:       "named entity check removed",
			from:       []dsl.TableCheck{{Name: "total_positive", Expr: "total > 0"}},
			constraint: "total_positive",
		},
		{
			name:       "unnamed entity check added",
			to:         []dsl.TableCheck{{Expr: "total > 0"}},
			constraint: unnamedCheckName(entityWithChecks(nil, ""), "total > 0", 1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldIR := &dsl.IR{Entities: []dsl.Entity{*entityWithChecks(tc.from, tc.fieldFrom)}}
			newIR := &dsl.IR{Entities: []dsl.Entity{*entityWithChecks(tc.to, tc.fieldTo)}}

			AssignProtoNumbers(oldIR, newIR)
			scripts, err := EmitSQL(oldIR, newIR, ComputeDiff(oldIR, newIR))
			if err != nil {
				t.Fatalf("EmitSQL: %v", err)
			}

			quoted := strconv.Quote(tc.constraint)
			if !strings.Contains(scripts.Up, quoted) {
				t.Errorf("up-migration does not name %s:\n%s", quoted, scripts.Up)
			}
			if !strings.Contains(scripts.Down, quoted) {
				t.Errorf("down-migration does not name %s, so it cannot undo what "+
					"up did:\n%s", quoted, scripts.Down)
			}

			// Exactly one of ADD/DROP on each side, in opposite senses.
			upAdds := strings.Contains(scripts.Up, "ADD CONSTRAINT "+quoted)
			downAdds := strings.Contains(scripts.Down, "ADD CONSTRAINT "+quoted)
			if upAdds == downAdds {
				t.Errorf("up and down both %s the constraint; the rollback is not a "+
					"rollback.\n--- up ---\n%s\n--- down ---\n%s",
					map[bool]string{true: "add", false: "drop"}[upAdds],
					scripts.Up, scripts.Down)
			}
		})
	}
}

// A Change whose class append does not recognise must not disappear.
//
// ChangeClass is an int, so a class this build does not know is simply an
// unlisted value — exactly what a future ClassDestructive (task #13) is until
// append learns it. Before the default clause such a change vanished: no bucket, so the plan reported no such
// change and EmitSQL had nothing to emit for it. The assertion is specifically
// that it lands somewhere and that somewhere is the strictest bucket — a change
// of unknown danger classified as safe is the failure mode worth preventing.
func TestAppendDoesNotDropAnUnknownClass(t *testing.T) {
	var d Diff
	d.append(Change{Kind: KindCheckAdded, Class: ChangeClass(99), EntityID: "app.user"})

	total := len(d.Additive) + len(d.BackfillRequired) + len(d.Breaking)
	if total != 1 {
		t.Fatalf("appended 1 change, found %d across all buckets — an unrecognised "+
			"class vanished from the diff, so the plan would report no change and "+
			"emit no DDL for it", total)
	}
	if len(d.Breaking) != 1 {
		t.Errorf("unknown class landed in additive/backfill, not breaking; a change " +
			"whose danger this build cannot assess must not be reported as safe")
	}
}
