package dsl

import (
	"strings"
	"testing"
)

// `check` is the only keyword valid both as a field modifier and as an entity
// member, and unlike `primary` and `unique` — separated by a following `by` —
// both spellings are the keyword followed by a STRING. Lookahead cannot tell
// them apart, so indentation does: a field's modifiers may wrap onto following
// lines, and those continuation lines are indented past the field. A `check` at
// or left of the field's own column is the next member, not a continuation.
//
// The rule formalises the convention the schemas already used. Across the
// corpus every continuation check sits under its field's type column and every
// entity-level check sits at member indent; the change reclassified none of
// them.
//
// Before the rule the modifier form always won after a field, and every
// consequence was silent:
//
//   - A table-level check written under a field became that field's, so the
//     generated constraint took that column's name even when the predicate
//     never mentioned it. Confirmed live: `check "length(name) > 0"` following
//     `colour text` emitted ADD CONSTRAINT "e2e_widget_colour_check".
//   - Two in a row collapsed. The parser built two ModCheckDecls and lowering
//     kept the last, discarding a constraint the author wrote — no diff entry,
//     no DDL, no diagnostic.
//   - `check "..." as <name>` was a parse error there, because the modifier
//     form does not consume `as`. The same line one position earlier parsed.

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

// The form that used to be a parse error in this position.
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

// Two entity-level checks after a field must both survive. Previously both were
// absorbed as the field's modifiers and lowering kept only the second, so the
// first vanished with no diagnostic.
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

// The constraint that ruled out every simpler fix: a field's modifiers may
// continue on the next line. atlprint's TestMultiLineField covers it and
// schema/vendor/internal/shopify/schema.atl uses it, so a rule keyed on "starts
// a line" would break the shipped corpus. Keyed on column, it does not.
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

// A field carries one check. A second used to replace the first in silence.
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

// The example in docs/reference/dsl-grammar.md must actually behave as it
// claims. A grammar reference that is wrong about binding is worse than one
// that says nothing, because the reader has no reason to doubt it.
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
// `check` inside them can only be a modifier and there is no ambiguity to
// resolve. Applying the column rule there turned an ordinary declaration into
// a hard parse error — "expected arg name or '}', got check" — which says
// nothing about binding or indentation, so the fix (indent one more column)
// was unguessable. The shipped corpus escaped only because
// schema/vendor/internal/shopify/schema.atl:426 happens to be indented deeper
// than its arg.
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

// Entity-level checks in positions where no field precedes them.
//
// An earlier version of this file carried this test with the note "this test is
// here so the next person does not make it" — about the argument that
// entity-level checks are unreachable, which was used to justify a
// constraint-naming change. Rewriting the file for the new rule dropped it,
// taking with it the only coverage of `check` as the first entity member, after
// `table "..."`, and after `ttl_field`. Restored.
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

// The error message tells the author what to do; following it literally has to
// work. Anything else is a dead end dressed as guidance.
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
