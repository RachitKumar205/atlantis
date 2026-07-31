package codegen

import "sort"

// Which IR attributes the differ actually reads, and which it does not.
//
// The differ enumerates the IR's attributes by hand, so it drifts from the IR
// silently. CHECK constraints, CompositePK, TouchOnUpdateField, Identity and
// Primary all went uncovered *simultaneously* — not because any one was hard,
// but because nothing was watching.
//
// That matters more than it looks. The product's differentiating claim is that
// its safety checks derive from the schema diff and are therefore complete over
// it, as against hand-written checks that are incomplete by construction.
// **"Complete over the diff" is only true if the diff is complete.** A
// migration adding a CHECK, run through a differ that never reads CHECKs,
// emits DDL containing no CHECK — and a rehearsal probe examining that DDL
// reports `pass` for a constraint that was never created.
//
// This registry lives in a non-test file on purpose. UncoveredIRAttributes is
// meant to feed a runtime rule forcing verdict `unverified` when a plan touches
// an attribute the diff cannot see. **That rule does not exist yet** — the
// rehearsal verdict it would set is not built. Nothing calls
// UncoveredIRAttributes today except its own test.
//
// It is here rather than in a _test.go file so that when the verdict lands, the
// backstop is a wiring change instead of a rewrite. Being unwired is a state
// that can be fixed; being unreachable from production is a design that cannot.
//
// diff_completeness_test.go asserts the registry is exhaustive and honest.

type coverageKind int

const (
	// diffed: a differ reads this field and emits a Change when it moves.
	diffed coverageKind = iota
	// notSchema: changing it changes nothing the caller can observe — no DDL
	// and no generated query — so there is nothing for a differ to detect.
	notSchema
	// gap: it DOES affect generated output, no differ reads it, and a change to
	// it is therefore invisible.
	//
	// "Generated output" rather than "DDL" on purpose. SoftDeleteField emits no
	// DDL at all; it changes the WHERE clause of every generated read. A
	// migration that adds it alters what the application returns while the
	// diff, and therefore rehearsal, reports nothing changed — which is the same
	// failure as a missed constraint arriving by a different route.
	//
	// Every gap is intended to force verdict `unverified` at rehearsal (see
	// UncoveredIRAttributes, and note that consumer does not exist yet), and
	// names the task that closes it. A gap is a debt with a due date, not a
	// shrug.
	gap
)

type coverage struct {
	kind coverageKind
	// by names the differ function, for kind==diffed. Verified to exist.
	by string
	// why explains a notSchema or gap entry. Required; "not needed" is not a
	// reason.
	why string
}

var entityCoverage = map[string]coverage{
	"Name":               {notSchema, "", "identity — a change is a rename, which the entity-level walk handles as drop+create"},
	"Namespace":          {notSchema, "", "identity — same as Name"},
	"Kind":               {gap, "", "entity vs hypertable changes the physical table; no differ reads it (task #12 follow-up)"},
	"Fields":             {diffed, "diffFields", ""},
	"Indexes":            {diffed, "diffIndexes", ""},
	"Uniques":            {diffed, "diffUniques", ""},
	"Cache":              {diffed, "diffCache", ""},
	"QueryTimeoutMS":     {diffed, "diffQueryTimeout", ""},
	"TableName":          {diffed, "diffTableName", ""},
	"Checks":             {diffed, "diffChecks", ""},
	"CompositePK":        {gap, "", "changing a composite PK rewrites the table; no differ reads it (task #12 follow-up)"},
	"TouchOnUpdateField": {gap, "", "emits a BEFORE UPDATE trigger; a change adds or drops it undetected (task #12 follow-up)"},
	"SoftDeleteField":    {gap, "", "changes generated query predicates; not DDL, but a silent behaviour change (task #12 follow-up)"},
	"PartitionField":     {gap, "", "tenant isolation; already fails open in the dispatcher and its RLS replacement is defeated — task #29"},
	"TimeField":          {gap, "", "hypertable time column; embedded strips Timescale DDL anyway, so a change is doubly invisible (task #12 follow-up)"},
	"TtlField":           {gap, "", "drives the TTL sweeper, whose DELETE is MySQL syntax and silently deletes nothing (task #12 follow-up)"},
	// Not a gap: a gap forces `unverified`, and doing that for a clause with no
	// runtime effect would degrade every verdict to buy no safety. has_many /
	// has_one / via parse, validate and diff-walk while emitting nothing — task
	// #14 deletes them from the grammar, at which point this entry goes too.
	"Relations": {notSchema, "", "has_many/has_one/via parse and emit nothing — no DDL, no generated query; task #14 removes them from the grammar"},
	// Emits no DDL, but it is not inert: proto.go renders it as `reserved N;`
	// in the generated .proto. Classified notSchema because the boundary this
	// registry polices is the database schema — the thing a migration changes
	// and a rehearsal proves safe. Wire compatibility is a real concern with a
	// real failure mode, and it needs its own completeness check rather than
	// borrowing this one.
	"RetiredProtoNumbers": {notSchema, "", "wire-stability bookkeeping; emits no DDL and changes no generated query — it is rendered into the .proto, which is a separate completeness question"},
}

var fieldCoverage = map[string]coverage{
	"Name":     {notSchema, "", "identity — a change is add+drop, handled by diffFields' name-keyed walk"},
	"Type":     {diffed, "diffFields", ""},
	"NotNull":  {diffed, "diffFields", ""},
	"Unique":   {diffed, "diffFields", ""},
	"Default":  {diffed, "diffFields", ""},
	"Ref":      {diffed, "diffFields", ""},
	"Serial":   {diffed, "diffFields", ""},
	"Backfill": {diffed, "diffFields", ""},
	"Check":    {diffed, "diffChecks", ""},
	"Primary":  {gap, "", "moving the primary key rewrites the table; no differ reads it (task #12 follow-up)"},
	"Identity": {gap, "", "GENERATED ... AS IDENTITY differs from SERIAL in DDL; no differ reads it (task #12 follow-up)"},
	// Same boundary as RetiredProtoNumbers: no DDL, no query change. Worth
	// stating plainly that "never authored" is not the reason — a change here
	// would be a wire break, and it is out of scope for this registry rather
	// than harmless.
	"ProtoNumber": {notSchema, "", "assigned by codegen; emits no DDL and changes no generated query. A change would be a wire break, which is a separate concern from migration safety"},
}

// UncoveredIRAttributes lists the IR attributes the differ cannot see. Any plan
// touching one of them must be reported `unverified` rather than `pass`,
// because no amount of examining the generated DDL can reveal a change that
// never entered the diff.
//
// This has no caller yet — the rehearsal verdict is Phase 2 work. Derived from
// the coverage map rather than hand-listed so that when it is wired, it cannot
// disagree with the map the tests police.
func UncoveredIRAttributes() []string {
	var out []string
	for name, c := range entityCoverage {
		if c.kind == gap {
			out = append(out, "Entity."+name)
		}
	}
	for name, c := range fieldCoverage {
		if c.kind == gap {
			out = append(out, "Field."+name)
		}
	}
	sort.Strings(out)
	return out
}
