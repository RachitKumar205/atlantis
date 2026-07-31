package dsl

import "testing"

// A characterization test: it pins what the parser does today, which is not
// what the grammar says. Task #31 decides the fix; this exists so that fix
// arrives as a deliberate, visible change rather than as a surprise.
//
// `check` is the only keyword valid both as a field modifier and as an entity
// member. `primary` and `unique` have the same overlap but are separated by a
// following `by`; both spellings of `check` are the keyword followed by a
// STRING, so lookahead cannot tell them apart and the grammar is
// newline-insensitive.
//
// The modifier form wins **whenever the preceding member was a field**, because
// parseFieldModifiers is still consuming and does not stop at a newline. It
// does not win otherwise: a `check` following `table "..."`, `ttl_field`,
// `unique by`, `soft_delete by`, `cache`, or appearing as the first member,
// reaches parseTableCheckDecl and lands in e.Checks correctly — including the
// `as <name>` form. TestTableCheckParsesWhenNotPrecededByAField pins that.
//
// So the defect is narrower than "entity-level checks are unreachable", and
// correspondingly nastier: whether a given line means an entity constraint or a
// field modifier depends on what precedes it, with no diagnostic either way.
//
// Consequences:
//
//   - A check written after a field silently becomes that field's, so it is
//     attributed to a column its predicate may not mention, and the generated
//     constraint takes that column's name.
//   - `check "..." as <name>` after a field is a parse error, because the
//     modifier form does not consume `as` and the dangling token reaches the
//     member dispatcher. The same line one position earlier parses fine.
//   - Moving a check line across a field boundary changes which of the two
//     things it means, and therefore renames or re-attributes the constraint —
//     from a source edit that looks purely cosmetic.
//
// The tempting fix (treat a `check` that starts a line as table-level) is
// wrong: a field's modifiers may wrap onto a continuation line, atlprint's
// TestMultiLineField covers it, and schema/vendor/internal/shopify/schema.atl
// uses it. TestFieldModifiersMayWrapOntoTheNextLine below pins that, so the
// constraint on #31's solution is checked in rather than remembered.
func TestTableLevelCheckIsAbsorbedByThePrecedingField(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  name text not null\n" +
		"  colour text\n" +
		"  check \"length(name) > 0\"\n" +
		"}\n"

	e := lowerOne(t, src)

	if len(e.Checks) != 0 {
		t.Fatalf("Checks = %v — the parser now reaches the entity-level form. "+
			"That is the fix task #31 describes; update this test to assert the "+
			"correct behaviour rather than the old one.", e.Checks)
	}

	var owner string
	for i := range e.Fields {
		if e.Fields[i].Check != "" {
			owner = e.Fields[i].Name
		}
	}
	if owner != "colour" {
		t.Fatalf("the check landed on %q, want colour (the field declared above "+
			"it). If this changed, attribution changed, and the generated "+
			"constraint name changed with it.", owner)
	}

	// The naming consequence, stated as an assertion rather than a comment: the
	// expression mentions `name` and nothing else, yet the constraint is named
	// after `colour`, purely because of line order.
	if e.Fields[len(e.Fields)-1].Name != "colour" {
		t.Fatal("fixture drifted: colour must be the last field for this to hold")
	}
}

// `check "..." as <name>` does not parse *after a field*. Asserted so that
// #31's fix has an unambiguous done condition.
//
// It parses fine elsewhere — see TestTableCheckParsesWhenNotPrecededByAField.
// An earlier version of this comment claimed the form was unreachable outright,
// which was wrong and made the naming decisions built on top of it wrong too.
func TestNamedTableCheckDoesNotParseAfterAField(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  name text not null\n" +
		"  check \"length(name) > 0\" as name_nonempty\n" +
		"}\n"

	if _, err := Parse("t.atl", []byte(src)); err == nil {
		t.Fatal("`check \"...\" as <name>` after a field parses now — task #31 is " +
			"done. Replace this test with one asserting the name reaches " +
			"e.Checks[0].Name.")
	}
}

// The other half of the truth: entity-level checks are perfectly reachable when
// the preceding member is not a field.
//
// This matters beyond the grammar. Reasoning that entity-level checks could not
// exist was used to argue that changing how unnamed ones are named was free —
// no deployed database could hold one. That argument was false, and this test
// is here so the next person does not make it.
func TestTableCheckParsesWhenNotPrecededByAField(t *testing.T) {
	for name, src := range map[string]string{
		"after a table override": "entity W in e2e {\n  id bigint primary\n  name text not null\n  table \"public.w\"\n  check \"length(name) > 0\"\n}\n",
		"as the first member":    "entity W in e2e {\n  check \"1 = 1\"\n  id bigint primary\n}\n",
		"after ttl_field":        "entity W in e2e {\n  id bigint primary\n  ts timestamptz not null\n  ttl_field ts\n  check \"id > 0\" as id_positive\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := lowerOne(t, src)
			if len(e.Checks) != 1 {
				t.Fatalf("Checks = %v, want exactly one entity-level check. "+
					"Entity-level checks ARE reachable; any argument that relies on "+
					"them being impossible is unsound.", e.Checks)
			}
			for i := range e.Fields {
				if e.Fields[i].Check != "" {
					t.Errorf("field %s absorbed it as %q", e.Fields[i].Name, e.Fields[i].Check)
				}
			}
		})
	}
}

// The constraint on #31: a field's modifiers may continue on the next line.
// This is why the line-position rule was tried and reverted — it broke this
// form, atlprint's TestMultiLineField, and the shipped vendor schema.
func TestFieldModifiersMayWrapOntoTheNextLine(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  status varchar(20) not null\n" +
		"    check \"status IN ('active','done')\"\n" +
		"}\n"

	e := lowerOne(t, src)

	var got string
	for i := range e.Fields {
		if e.Fields[i].Name == "status" {
			got = e.Fields[i].Check
		}
	}
	if got != "status IN ('active','done')" {
		t.Errorf("wrapped modifier did not bind to status: Check = %q, Checks = %v.\n"+
			"  Any disambiguation of table-level check must keep this working — "+
			"schema/vendor/internal/shopify/schema.atl relies on it.", got, e.Checks)
	}
}

func lowerOne(t *testing.T, src string) *Entity {
	t.Helper()
	f, err := Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse:\n%s  %v", src, err)
	}
	ir, err := Lower([]*File{f})
	if err != nil {
		t.Fatalf("lower:\n%s  %v", src, err)
	}
	if len(ir.Entities) != 1 {
		t.Fatalf("got %d entities, want 1", len(ir.Entities))
	}
	return &ir.Entities[0]
}
