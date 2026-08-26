package dsl

import (
	"strings"
	"testing"
)

// `check` is valid both as a field modifier and as an entity member, and both
// spellings are the keyword followed by a STRING. Indentation separates them: a
// `check` at or left of the field's own column is the next member, not a
// continuation of the field's modifiers.
//
// The tests below pin the three failures the modifier form produces where it
// wins after a field, none of which report anything:
//
//   - A table-level check becomes that field's, so the constraint takes the
//     column's name. `check "length(name) > 0"` after `colour text` emits
//     ADD CONSTRAINT "e2e_widget_colour_check".
//   - Two in a row collapse. The parser builds two ModCheckDecls and lowering
//     keeps the last: no diff entry, no DDL, no diagnostic.
//   - `check "..." as <name>` is a parse error, since the modifier form does
//     not consume `as`.

func TestCheckAtMemberIndentBelongsToTheEntity(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  name text not null\n" +
		"  colour text\n" +
		"  check \"length(name) > 0\"\n" +
		"}\n"

	e := lowerOne(t, src)

	if len(e.Checks) != 1 || e.Checks[0].Expr != "length(name) > 0" {
		t.Fatalf("Checks = %v, want the one entity-level check. At member indent "+
			"this is the entity's constraint, not the preceding field's.", e.Checks)
	}
	for i := range e.Fields {
		if e.Fields[i].Check != "" {
			t.Errorf("field %s absorbed it as %q — the constraint would be named "+
				"after %s, a column the predicate never mentions",
				e.Fields[i].Name, e.Fields[i].Check, e.Fields[i].Name)
		}
	}
}

// The modifier form does not consume `as`, so this parses only as a member.
func TestNamedCheckParsesAfterAField(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  name text not null\n" +
		"  check \"length(name) > 0\" as name_nonempty\n" +
		"}\n"

	e := lowerOne(t, src)
	if len(e.Checks) != 1 {
		t.Fatalf("Checks = %v, want one", e.Checks)
	}
	if e.Checks[0].Name != "name_nonempty" {
		t.Errorf("Name = %q, want name_nonempty", e.Checks[0].Name)
	}
}

// Two entity-level checks after a field both survive. Absorbed as that field's
// modifiers, lowering keeps only the second and the first vanishes silently.
func TestTwoChecksAfterAFieldBothSurvive(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  n text not null\n" +
		"  check \"id > 0\"\n" +
		"  check \"id < 100\"\n" +
		"}\n"

	e := lowerOne(t, src)
	if len(e.Checks) != 2 {
		t.Fatalf("Checks = %v, want 2 — one of them used to be discarded silently, "+
			"so it produced no diff entry and no DDL", e.Checks)
	}
	got := []string{e.Checks[0].Expr, e.Checks[1].Expr}
	if got[0] != "id > 0" || got[1] != "id < 100" {
		t.Errorf("Checks = %v, want both predicates in declaration order", got)
	}
}

// A field's modifiers may continue on the next line. atlprint's
// TestMultiLineField covers the shape and the shipped corpus uses it, so a rule
// keyed on "starts a line" reclassifies existing declarations. Keyed on column,
// it does not.
func TestFieldModifiersMayWrapOntoTheNextLine(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  status varchar(20) not null\n" +
		"    check \"status IN ('active','done')\"\n" +
		"}\n"

	e := lowerOne(t, src)

	if len(e.Checks) != 0 {
		t.Errorf("an indented continuation check escaped to the entity as %v", e.Checks)
	}
	var got string
	for i := range e.Fields {
		if e.Fields[i].Name == "status" {
			got = e.Fields[i].Check
		}
	}
	if got != "status IN ('active','done')" {
		t.Errorf("wrapped modifier did not bind to status: Check = %q", got)
	}
}

// A check sharing its field's line binds to it. This needs no special case in
// the rule: parseFieldModifiers runs only after the name and type are consumed,
// so anything left on that line is at a greater column than the field name.
// Asserted because that reasoning is the only thing standing behind it.
func TestSameLineCheckBindsToItsField(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  position int not null default 1 check \"position BETWEEN 1 AND 3\"\n" +
		"}\n"

	e := lowerOne(t, src)
	if len(e.Checks) != 0 {
		t.Errorf("a same-line check escaped to the entity as %v", e.Checks)
	}
	var got string
	for i := range e.Fields {
		if e.Fields[i].Name == "position" {
			got = e.Fields[i].Check
		}
	}
	if got != "position BETWEEN 1 AND 3" {
		t.Errorf("Check = %q, want the same-line predicate", got)
	}
}

// A field indented deeper than usual still owns a check indented deeper still.
// The rule is relative to the field, not to a fixed column, so it holds inside
// a block whatever its nesting.
func TestTheRuleIsRelativeToTheField(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"      deep_field text not null\n" +
		"        check \"deep_field <> ''\"\n" +
		"  shallow text\n" +
		"  check \"shallow IS NOT NULL\"\n" +
		"}\n"

	e := lowerOne(t, src)

	var deep string
	for i := range e.Fields {
		if e.Fields[i].Name == "deep_field" {
			deep = e.Fields[i].Check
		}
	}
	if deep != "deep_field <> ''" {
		t.Errorf("deeper check did not bind to its deeper field: %q", deep)
	}
	if len(e.Checks) != 1 || e.Checks[0].Expr != "shallow IS NOT NULL" {
		t.Errorf("Checks = %v, want only the member-indent one", e.Checks)
	}
}

// A field carries one check; a second is an error, not a silent replacement.
func TestSecondCheckOnOneFieldIsAnError(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  n text not null\n" +
		"      check \"id > 0\"\n" +
		"      check \"id < 100\"\n" +
		"}\n"

	f, err := Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = Lower([]*File{f})
	if err == nil {
		t.Fatal("two checks on one field lowered without error; one of them is " +
			"being discarded, and a discarded constraint produces no diff entry " +
			"and no DDL")
	}
	if !strings.Contains(err.Error(), "more than one `check`") {
		t.Errorf("error does not explain the problem: %v", err)
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

// Tab-indented schemas. Columns count bytes, so a tab is one column; the rule
// therefore works on tabs exactly as on spaces, provided a file does not mix
// them. The corpus uses spaces, so this is the case no existing schema covers.
func TestTheRuleWorksWithTabIndentation(t *testing.T) {
	src := "entity W in e2e {\n" +
		"\tid bigint primary\n" +
		"\tstatus varchar(20) not null\n" +
		"\t\tcheck \"status <> ''\"\n" +
		"\tcheck \"id > 0\" as id_positive\n" +
		"}\n"

	e := lowerOne(t, src)

	var status string
	for i := range e.Fields {
		if e.Fields[i].Name == "status" {
			status = e.Fields[i].Check
		}
	}
	if status != "status <> ''" {
		t.Errorf("the deeper tab-indented check did not bind to status: %q", status)
	}
	if len(e.Checks) != 1 || e.Checks[0].Name != "id_positive" {
		t.Errorf("Checks = %v, want the one at member indent", e.Checks)
	}
}

// The example in docs/reference/dsl-grammar.md binds the way that page says it
// does.
func TestGrammarReferenceExampleBindsAsDocumented(t *testing.T) {
	// Copied verbatim from the "Where a `check` binds" section.
	src := "entity Order in shop {\n" +
		"  id      bigint primary\n" +
		"  status  varchar(20) not null\n" +
		"          check \"status IN ('open','closed')\"\n" +
		"\n" +
		"  total   int not null\n" +
		"  qty     int not null\n" +
		"\n" +
		"  check \"total >= qty\" as total_covers_qty\n" +
		"}\n"

	e := lowerOne(t, src)

	var status string
	for i := range e.Fields {
		if e.Fields[i].Name == "status" {
			status = e.Fields[i].Check
		}
		if e.Fields[i].Name != "status" && e.Fields[i].Check != "" {
			t.Errorf("field %s picked up %q; the doc says only status has a field check",
				e.Fields[i].Name, e.Fields[i].Check)
		}
	}
	if status != "status IN ('open','closed')" {
		t.Errorf("status.Check = %q; the doc labels that line \"the field's\"", status)
	}

	if len(e.Checks) != 1 {
		t.Fatalf("Checks = %v, want exactly the one the doc labels \"the entity's\"", e.Checks)
	}
	if e.Checks[0].Name != "total_covers_qty" || e.Checks[0].Expr != "total >= qty" {
		t.Errorf("entity check = %+v, want total_covers_qty / total >= qty", e.Checks[0])
	}
}

// The rule must not leak into blocks that hold fields and nothing else.
//
// `args`, `state` and `ephemeral` have no entity-level `check` member, so a
// `check` inside them is always a modifier and there is no ambiguity to
// resolve.
//
// The column rule applied there makes an ordinary declaration a parse error —
// "expected arg name or '}', got check" — which names neither binding nor
// indentation, so the remedy of indenting one more column does not follow from
// it.
func TestChecksInFieldOnlyBlocksAreAlwaysModifiers(t *testing.T) {
	for name, src := range map[string]string{
		"job args": "job J in v {\n" +
			"  args {\n" +
			"    s varchar(20) not null\n" +
			"    check \"s IN ('a','b')\"\n" +
			"  }\n" +
			"  retries 2\n" +
			"  timeout 30m\n" +
			"}\n",
		"job args, indented deeper": "job J in v {\n" +
			"  args {\n" +
			"    s varchar(20) not null\n" +
			"      check \"s IN ('a','b')\"\n" +
			"  }\n" +
			"  retries 2\n" +
			"  timeout 30m\n" +
			"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse("t.atl", []byte(src)); err != nil {
				t.Errorf("did not parse: %v\n"+
					"  A check in a field-only block is unambiguously the field's, "+
					"at any indentation.", err)
			}
		})
	}
}

// Entity-level checks in the positions where no field precedes them: as the
// first member, after `table "..."`, and after `ttl_field`.
//
// The indentation rule never applies to these, so they are the cases that show
// an entity-level check is reachable at all.
func TestTableCheckParsesWithoutAPrecedingField(t *testing.T) {
	for name, src := range map[string]string{
		"after a table override": "entity W in e2e {\n  id bigint primary\n  name text not null\n  table \"public.w\"\n  check \"length(name) > 0\"\n}\n",
		"as the first member":    "entity W in e2e {\n  check \"1 = 1\"\n  id bigint primary\n}\n",
		"after ttl_field":        "entity W in e2e {\n  id bigint primary\n  ts timestamptz not null\n  ttl_field ts\n  check \"id > 0\" as id_positive\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := lowerOne(t, src)
			if len(e.Checks) != 1 {
				t.Fatalf("Checks = %v, want exactly one entity-level check", e.Checks)
			}
			for i := range e.Fields {
				if e.Fields[i].Check != "" {
					t.Errorf("field %s absorbed it as %q", e.Fields[i].Name, e.Fields[i].Check)
				}
			}
		})
	}
}

// Two identical predicates on one field are still two declarations.
//
// Treating them as a harmless duplicate — skipping the second instead of
// erroring — leaves the whole suite green while reintroducing the silent
// collapse this guard exists to stop. codegen already treats repeated
// predicates as separate constraints, which is why unnamedCheckName takes an
// occurrence counter.
func TestTwoIdenticalChecksOnOneFieldIsAlsoAnError(t *testing.T) {
	src := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  n text not null\n" +
		"      check \"id > 0\"\n" +
		"      check \"id > 0\"\n" +
		"}\n"

	f, err := Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Lower([]*File{f}); err == nil {
		t.Fatal("two identical checks on one field lowered without error; one is " +
			"being dropped, and Postgres would have held two constraints")
	}
}

// The duplicate-check error names a remedy. Applying it literally parses and
// lowers.
func TestTheDuplicateCheckAdviceActuallyWorks(t *testing.T) {
	// The advice: keep one on the field, move the other to member indentation
	// with a name.
	fixed := "entity W in e2e {\n" +
		"  id bigint primary\n" +
		"  n text not null\n" +
		"      check \"id > 0\"\n" +
		"  check \"id < 100\" as id_bounded\n" +
		"}\n"

	e := lowerOne(t, fixed)

	var fieldCheck string
	for i := range e.Fields {
		if e.Fields[i].Name == "n" {
			fieldCheck = e.Fields[i].Check
		}
	}
	if fieldCheck != "id > 0" {
		t.Errorf("field check = %q, want the one left in place", fieldCheck)
	}
	if len(e.Checks) != 1 || e.Checks[0].Name != "id_bounded" {
		t.Errorf("Checks = %v, want the relocated one named id_bounded", e.Checks)
	}
}
