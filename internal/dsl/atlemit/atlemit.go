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
	for i := range e.Fields {
		types[i] = renderType(&e.Fields[i].Type)
		nameW = max(nameW, len(e.Fields[i].Name))
		typeW = max(typeW, len(types[i]))
	}

	for i := range e.Fields {
		f := &e.Fields[i]
		fmt.Fprintf(&b, "  %-*s %-*s", nameW, f.Name, typeW, types[i])
		for _, m := range modifiers(f) {
			b.WriteString(" ")
			b.WriteString(m)
		}
		b.WriteString("\n")
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

// renderType turns a resolved type back into its surface spelling.
func renderType(t *dsl.FieldType) string {
	if t.Array {
		if t.Elem != nil {
			return renderType(t.Elem) + "[]"
		}
		return "text[]"
	}
	switch t.Name {
	case "varchar":
		if t.Len > 0 {
			return "varchar(" + strconv.Itoa(t.Len) + ")"
		}
		return "varchar"
	case "numeric":
		if t.HasNumP {
			return "numeric(" + strconv.Itoa(t.NumP) + ", " + strconv.Itoa(t.NumS) + ")"
		}
		return "numeric"
	case "vector":
		if t.VecDim > 0 {
			return "vector(" + strconv.Itoa(t.VecDim) + ")"
		}
		return "vector"
	default:
		return t.Name
	}
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
