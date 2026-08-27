// Package atlemit renders a dsl.Entity back into .atl source text.
//
// The IR→.atl printer, used for onboarding: declarations describing an existing
// database, as a starting point to review and commit. internal/dsl/atlprint
// solves the opposite problem, splicing a change into .atl text that already
// exists.
//
// Columns, types, keys, defaults, foreign keys, UNIQUE and CHECK constraints
// are read from the catalogue: FromPostgres supplies the first group, and
// introspect.Enrich the rest, which FromPostgres carries from a declaration
// that on this path is a stub holding none.
//
// Secondary indexes are read where .atl can spell them, which leaves
// expression, partial, bare-unique and non-btree indexes undeclared. Enrich
// returns a note naming each, and Entity emits a header comment saying the
// absence of an `index by` line means "not discovered", not "not present" —
// a file committed as complete produces a later CREATE INDEX for an index the
// database already has.
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
	b.WriteString("// Columns, types, keys, defaults, foreign keys, UNIQUE and CHECK\n")
	b.WriteString("// constraints and btree secondary indexes were read from the catalogue.\n")
	b.WriteString("// An index this cannot spell — on an expression, partial, unique with no\n")
	b.WriteString("// constraint behind it, or not btree — is named in the warnings instead,\n")
	b.WriteString("// and the first plan will propose creating one that already exists.\n")
	b.WriteString("// Review before committing.\n")

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

	// Constraints spanning more than one column, which have no field to hang
	// off. Single-column uniqueness is already a field modifier, and emitting
	// it again here would declare the same constraint twice.
	for _, u := range e.Uniques {
		if len(u.Fields) < 2 {
			continue
		}
		fmt.Fprintf(&b, "  unique by %s\n", strings.Join(u.Fields, ", "))
	}
	for _, c := range e.Checks {
		if c.Name != "" {
			fmt.Fprintf(&b, "  check %q as %s\n", c.Expr, c.Name)
			continue
		}
		fmt.Fprintf(&b, "  check %q\n", c.Expr)
	}

	// Only the btree indexes introspection can spell. A column carries `desc`
	// where the catalogue records a descending sort: an index on
	// (created_at DESC) is a different index from one on (created_at), so
	// dropping the modifier declares something the database does not have.
	for _, ix := range e.Indexes {
		if ix.Kind != dsl.IndexBtree || len(ix.Fields) == 0 {
			continue
		}
		cols := make([]string, len(ix.Fields))
		for i, f := range ix.Fields {
			cols[i] = f.Name
			if f.Desc {
				cols[i] += " desc"
			}
		}
		fmt.Fprintf(&b, "  index by %s\n", strings.Join(cols, ", "))
	}

	// Columns whose type has no .atl spelling are omitted and named. Silently
	// dropping one would hand the customer a declaration that looks complete
	// and is not — and the first thing that would tell them is an apply
	// planning to drop a column that is still in use.
	if len(dropped) > 0 {
		b.WriteString("\n  // NOT DECLARED — atlantis has no usable .atl type for these:\n")
		for _, d := range dropped {
			fmt.Fprintf(&b, "  //   %s\n", d)
		}
		b.WriteString("  //\n")
		b.WriteString("  // They exist in the database and this entity does not declare them.\n")
		b.WriteString("  // READ THIS BEFORE APPROVING THE FIRST PLAN: adopt baselines what the\n")
		b.WriteString("  // catalogue actually contains, not this file, so the next `tide plan`\n")
		b.WriteString("  // reports each one as a REMOVAL and the apply parks the column —\n")
		b.WriteString("  // renaming it to <name>__parked, dropping its NOT NULL, and scheduling\n")
		b.WriteString("  // it for deletion in 30 days. Anything still reading those columns\n")
		b.WriteString("  // breaks at that moment. Reject that plan unless you mean it.\n")
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
// Returning the introspected Postgres spelling for anything unrecognised emits
// files that do not parse:
//
//   - float8 canonicalises to "double precision", two tokens. The parser takes
//     one identifier, so `lat double precision not null` reads `double` as the
//     type and `precision` as the next field's name. canonicalUDT returns the
//     .atl spelling, since every other consumer compares that name as a raw
//     string.
//   - Arrays come back as `text[]`, the Postgres suffix form. The grammar is
//     the prefix form `[]text`.
//   - An unbounded varchar has no spelling where the parser demands varchar(N).
//     `text` parses and is wrong: text and varchar are different Postgres
//     types, so the generated file disagrees permanently with the catalogue it
//     came from. The grammar accepts bare `varchar`.
//
// A spelling this cannot guarantee returns false and the caller drops the
// column, naming it. A missing column in a generated starting point is
// recoverable; a file that does not parse is not.
func renderType(t *dsl.FieldType) (string, bool) {
	if t.Array {
		if t.Elem == nil {
			return "", false
		}
		// Prefix form: docs/reference/dsl-grammar.md maps []T in .atl to T[] in
		// Postgres.
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
		// pgvector allows `vector` with no dimension. parseType calls
		// p.expect(TokLParen) for this name with no optional branch, so bare
		// `vector` does not parse.
		return "", false
	}
	if toolchainHandles(t) {
		return t.Name, true
	}
	return "", false
}

// toolchainHandles reports whether the whole pipeline can carry a column of
// this type, not merely whether the parser accepts the spelling.
//
// It asks coltype rather than consulting a list. The parser accepts any
// identifier as a type name, so a whitelist testing whether a spelling lexes as
// one token admits everything; one such list drifted to hold six names the rest
// of the toolchain does not implement — integer, bool, timestamp, time, json,
// inet — each a spelling canonicalUDT hands back verbatim for an ordinary
// legacy column.
//
// That failure lands late. schema.SQLType renders TIMESTAMP, JSON, INET and
// TIME, so parse, lower, plan and apply are clean and the checkpoint is
// written; `tide codegen` is the first step to fail, with
// `unsupported type "timestamp" for proto`.
func toolchainHandles(t *dsl.FieldType) bool {
	pt, err := coltype.ProtoType(*t)
	if err != nil {
		return false
	}
	// GoType has no error return; `any` is its unknown-type fallback, and a
	// column typed `any` in a generated struct is not a column the customer
	// can use.
	if coltype.GoType(*t, true) == "any" {
		return false
	}
	// A well-known type is usable only if the .proto emitter imports its
	// definition, and it imports timestamp.proto and nothing else.
	//
	// coltype alone is not enough to see this. For `interval` ProtoType returns
	// google.protobuf.Duration with a nil error and GoType returns
	// time.Duration, and the emitted .proto then fails protoc with
	// `"google.protobuf.Duration" is not defined` — duration.proto appears
	// nowhere in internal/codegen.
	if strings.HasPrefix(pt, wellKnownPrefix) && pt != wellKnownTimestamp {
		return false
	}
	return true
}

const (
	wellKnownPrefix    = "google.protobuf."
	wellKnownTimestamp = "google.protobuf.Timestamp"
)

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
