// Package atlemit renders a dsl.Entity back into .atl source text.
//
// # Why this is not internal/dsl/atlprint
//
// atlprint splices a change into .atl text that already exists, preserving
// every byte it did not touch. Its package doc opens by stating the constraint
// this package exists to lift: "There is no IR→.atl printer." The two solve
// opposite problems — atlprint keeps a human's formatting, this one has no
// human formatting to keep — and merging them would blur a boundary that doc
// defends at length.
//
// # What this is for
//
// Onboarding. Somebody points atlantis at a database they already have, and
// atlantis writes the declarations describing it, so they are not asked to
// hand-write a file per table before they can adopt. The output is a starting
// point they review and commit, not an answer.
//
// # What it cannot tell them
//
// Introspection does not read indexes, uniques or CHECK predicates back from
// the catalogue — FromPostgres carries those over from whatever declaration it
// was given, and here it is given a stub with none. So a generated file
// UNDERSTATES the database: the columns, types, keys, defaults and foreign
// keys are real, and the absence of an `index by` line means "not discovered",
// never "not present".
//
// That is why Entity carries a header comment saying so. A customer who
// commits this file believing it complete will later add an index the database
// already has, and atlantis will emit a CREATE INDEX that fails or duplicates.
// Saying it in the file is cheaper than saying it in a postmortem.
package atlemit

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/codegen/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Entity renders one entity as a complete .atl declaration.
//
// The physical table is always named explicitly. An entity with no `table`
// clause resolves to atlantis.<namespace>_<name> (see introspect.physical),
// which will not match a table that already existed under any other name — and
// a generated declaration that points at the wrong table is worse than none,
// because it reads as correct.
func Entity(e *dsl.Entity, physicalTable string) string {
	var b strings.Builder

	b.WriteString("// Generated from the live database.\n")
	b.WriteString("//\n")
	b.WriteString("// Columns, types, keys, defaults and foreign keys were read from the\n")
	b.WriteString("// catalogue. Indexes, unique constraints and CHECK predicates were NOT:\n")
	b.WriteString("// atlantis cannot yet read those back, so their absence here means \"not\n")
	b.WriteString("// discovered\", not \"not present\". Review before committing.\n")

	fmt.Fprintf(&b, "entity %s in %s {\n", e.Name, e.Namespace)
	if physicalTable != "" {
		fmt.Fprintf(&b, "  table %q\n", physicalTable)
	}

	// Column width so modifiers line up, which is what the hand-written files
	// in this repo do. Computed over the fields actually being emitted rather
	// than fixed, so a table of short names does not get a wall of padding.
	nameW, typeW := 0, 0
	types := make([]string, len(e.Fields))
	renderable := make([]bool, len(e.Fields))
	var dropped []string
	for i := range e.Fields {
		types[i], renderable[i] = renderType(&e.Fields[i].Type)
		if !renderable[i] {
			dropped = append(dropped, fmt.Sprintf("%s (%s)", e.Fields[i].Name, e.Fields[i].Type.Name))
			continue
		}
		nameW = max(nameW, len(e.Fields[i].Name))
		typeW = max(typeW, len(types[i]))
	}

	for i := range e.Fields {
		if !renderable[i] {
			continue
		}
		f := &e.Fields[i]
		fmt.Fprintf(&b, "  %-*s %-*s", nameW, f.Name, typeW, types[i])
		for _, m := range modifiers(f) {
			b.WriteString(" ")
			b.WriteString(m)
		}
		b.WriteString("\n")
	}

	// Columns whose type has no .atl spelling are omitted and named. Silently
	// dropping one would hand the customer a declaration that looks complete
	// and is not — and the first thing that would tell them is an apply
	// planning to drop a column that is still in use.
	if len(dropped) > 0 {
		b.WriteString("\n  // NOT DECLARED — no .atl type for these columns:\n")
		for _, d := range dropped {
			fmt.Fprintf(&b, "  //   %s\n", d)
		}
		b.WriteString("  // They exist in the database. Declaring this entity without them\n")
		b.WriteString("  // means atlantis does not manage them; it will not drop them.\n")
	}

	b.WriteString("}\n")
	return b.String()
}

// modifiers renders a field's modifiers in a fixed order.
//
// The grammar allows any order; this picks one and keeps it, so two runs over
// the same database produce byte-identical files and a customer re-running
// generation sees an empty git diff rather than a reshuffle.
func modifiers(f *dsl.Field) []string {
	var out []string
	if f.Primary {
		out = append(out, "primary")
	}
	if f.Identity {
		out = append(out, "identity")
	}
	if f.Serial {
		out = append(out, "serial")
	}
	// A primary key column is NOT NULL by construction in Postgres, and the
	// parser rejects nothing here — but emitting `primary not null` is noise
	// that no hand-written file in this repo contains.
	if f.NotNull && !f.Primary {
		out = append(out, "not null")
	}
	if f.Unique {
		out = append(out, "unique")
	}
	// `serial` and `default` are incompatible per the grammar, and introspect
	// already classifies a serial default as Serial rather than Default — but
	// a hand-edited IR could carry both, and emitting both produces a file
	// that will not parse.
	if f.Default != nil && !f.Serial {
		if d := renderDefault(f.Default); d != "" {
			out = append(out, "default "+d)
		}
	}
	if f.Ref != nil {
		out = append(out, renderRef(f.Ref))
	}
	if f.Check != "" {
		out = append(out, fmt.Sprintf("check %q", f.Check))
	}
	return out
}

// renderType turns a resolved type back into its .atl surface spelling, and
// reports whether it could.
//
// # Why it refuses rather than guessing
//
// The first version returned t.Name for anything it did not recognise, which
// is the introspected Postgres spelling. Three of those do not parse, and an
// adversarial review reproduced all three end to end:
//
//   - float8 canonicalised to "double precision", TWO tokens. The parser takes
//     one identifier, so `lat double precision not null` parsed `double` as
//     the type and `precision` as the next field's name. Fixed at the source —
//     canonicalUDT now returns the .atl spelling — rather than translated
//     here, because every OTHER consumer of that name compares it as a raw
//     string too, and a translation here would have left them all broken.
//   - Arrays came out `text[]`, the Postgres suffix form. The grammar is the
//     prefix form `[]text`; the suffix spelling is a syntax error.
//   - An unbounded varchar had no .atl spelling at all: the parser required
//     varchar(N) unconditionally. The first fix here emitted `text`, which
//     parses and is WRONG — text and varchar are different Postgres types, so
//     the generated file disagreed with the catalogue it was generated from,
//     permanently. The round trip caught it. The grammar now accepts bare
//     `varchar`, so this renders the type the column actually has.
//
// Every one produced a file the customer could not commit, and the error named
// a generated file rather than the table it came from. So this now emits only
// spellings it can guarantee, and the caller drops what it cannot render and
// says so. A missing column in a generated starting point is recoverable; a
// file that will not parse is not usable at all.
func renderType(t *dsl.FieldType) (string, bool) {
	if t.Array {
		if t.Elem == nil {
			return "", false
		}
		// Prefix form. docs/reference/dsl-grammar.md maps []T (.atl) to T[]
		// (Postgres); the first version emitted the right-hand column.
		elem, ok := renderType(t.Elem)
		if !ok {
			return "", false
		}
		return "[]" + elem, true
	}
	switch t.Name {
	case "varchar":
		if t.Len > 0 {
			return "varchar(" + strconv.Itoa(t.Len) + ")", true
		}
		return "varchar", true
	case "numeric":
		if t.HasNumP {
			return "numeric(" + strconv.Itoa(t.NumP) + ", " + strconv.Itoa(t.NumS) + ")", true
		}
		return "numeric", true
	case "vector":
		if t.VecDim > 0 {
			return "vector(" + strconv.Itoa(t.VecDim) + ")", true
		}
		return "vector", true
	}
	if toolchainHandles(t) {
		return t.Name, true
	}
	return "", false
}

// toolchainHandles reports whether the whole pipeline can carry a column of
// this type, not merely whether the parser accepts the spelling.
//
// # Why it asks coltype instead of consulting a list
//
// This was a hand-written whitelist, and the property it tested was the wrong
// one. Its comment justified itself as guarding against "a name that lexes as
// one token and still is not a type the parser accepts" — but the parser
// accepts ANY identifier as a type name, so that test admits everything, and
// the list quietly drifted to include six names the rest of the toolchain does
// not implement: integer, bool, timestamp, time, json and inet. Every one is a
// spelling canonicalUDT hands back verbatim for an ordinary legacy column.
//
// The consequence landed at the worst possible step. schema.SQLType renders
// TIMESTAMP, JSON, INET and TIME — all valid Postgres — so parse, lower, plan
// and apply were clean and the checkpoint was written. `tide codegen` was the
// first thing to fail, with `unsupported type "timestamp" for proto`, after
// the customer had committed the file and migrated the database.
//
// Asking coltype makes the question "can codegen emit this column" rather than
// "does this name look like one token", and there is then one list rather than
// two that drift.
func toolchainHandles(t *dsl.FieldType) bool {
	if _, err := coltype.ProtoType(*t); err != nil {
		return false
	}
	// GoType has no error return; `any` is its unknown-type fallback, and a
	// column typed `any` in a generated struct is not a column the customer
	// can use.
	return coltype.GoType(*t, true) != "any"
}

// renderDefault turns a resolved Default into a DefaultExpr.
//
// Read off parseDefaultValue in internal/dsl/parser.go, not off the grammar
// reference, which is wrong here in two ways: it documents string literals as
// single-quoted (the lexer rejects `'` outright) and it documents a bare
// FunctionCall (the parser wants `raw "<sql>"`). A round-trip test caught both
// on its first run, which is the argument for having one.
//
// Nothing is dropped. Every kind has a spelling, because `raw` accepts an
// arbitrary expression — so a default this function cannot render as a literal
// still survives verbatim rather than vanishing from a customer's schema.
func renderDefault(d *dsl.Default) string {
	switch d.Kind {
	case dsl.DefaultIRNow:
		return "now()"
	case dsl.DefaultIRInt:
		return strconv.FormatInt(d.Int, 10)
	case dsl.DefaultIRBool:
		return strconv.FormatBool(d.Bool)
	case dsl.DefaultIRString:
		return quote(d.Str)
	case dsl.DefaultIRFloat:
		// There is no float literal token — parseDefaultValue accepts TokInt
		// and nothing else numeric — so a fractional default goes through raw.
		return "raw " + quote(strconv.FormatFloat(d.Float, 'g', -1, 64))
	case dsl.DefaultIRRaw:
		return "raw " + quote(d.Str)
	default:
		// A kind this build does not know. Emitting nothing loses the default;
		// emitting a guess produces a file that will not parse, and the error
		// names a generated file rather than the table it came from. Losing it
		// is the recoverable half, and the header comment already tells the
		// reader this file is a starting point.
		return ""
	}
}

// quote renders a .atl string literal, which is double-quoted.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// renderRef turns a foreign key into a `references` modifier.
func renderRef(r *dsl.Ref) string {
	out := "references " + r.TargetID + "." + r.TargetField
	switch r.OnDelete {
	case dsl.RefActionCascade:
		out += " on delete cascade"
	case dsl.RefActionSetNull:
		out += " on delete set null"
	case dsl.RefActionRestrict:
		out += " on delete restrict"
	}
	return out
}

// File renders several entities into one .atl file, sorted by name so the
// output is stable across runs.
func File(entities []*dsl.Entity, physicalByID map[string]string) string {
	sorted := slices.Clone(entities)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID() < sorted[j].ID() })

	var b strings.Builder
	for i, e := range sorted {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(Entity(e, physicalByID[e.ID()]))
	}
	return b.String()
}
