package codegen

import "sort"

// Which IR attributes the differ actually reads, and which it does not.
//
// The differ enumerates the IR's attributes by hand, so it drifts from the IR
// silently. CHECK constraints, CompositePK, TouchOnUpdateField, Identity and
// Primary all went uncovered at once.
//
// The safety checks derive from the schema diff and are complete over it, which
// holds only if the diff is complete. A migration adding a CHECK, run through a
// differ that never reads CHECKs, emits DDL containing no CHECK, and a
// rehearsal probe examining that DDL reports `pass` for a constraint that was
// never created.
//
// UncoveredIRAttributes is meant to feed a runtime rule forcing verdict
// `unverified` when a plan touches an attribute the diff cannot see. That rule
// does not exist yet, and nothing calls UncoveredIRAttributes except its own
// test. It lives in a non-test file so that wiring it is a wiring change.
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
	// "Generated output" rather than "DDL": SoftDeleteField emits no DDL and
	// changes the WHERE clause of every generated read, so a migration adding
	// it alters what the application returns while the diff, and therefore
	// rehearsal, reports nothing changed.
	//
	// Every gap is meant to force verdict `unverified` at rehearsal — see
	// UncoveredIRAttributes, whose consumer does not exist yet — and names the
	// task that closes it.
	gap
)

type coverage struct {
	kind coverageKind
	// by names the differ function, for kind==diffed. Verified to exist.
	by string
	// why explains a notSchema or gap entry, and is required.
	why string
}

var entityCoverage = map[string]coverage{
	"Name":                {notSchema, "", "identity — a change is a rename, which the entity-level walk handles as drop+create"},
	"Namespace":           {notSchema, "", "identity — same as Name"},
	"Kind":                {gap, "", "entity vs hypertable changes the physical table; no differ reads it (task #12 follow-up)"},
	"Fields":              {diffed, "diffFields", ""},
	"Indexes":             {diffed, "diffIndexes", ""},
	"Uniques":             {diffed, "diffUniques", ""},
	"Cache":               {diffed, "diffCache", ""},
	"QueryTimeoutMS":      {diffed, "diffQueryTimeout", ""},
	"TableName":           {diffed, "diffTableName", ""},
	"Checks":              {diffed, "diffChecks", ""},
	"CompositePK":         {gap, "", "changing a composite PK rewrites the table; no differ reads it (task #12 follow-up)"},
	"TouchOnUpdateField":  {gap, "", "emits a BEFORE UPDATE trigger; a change adds or drops it undetected (task #12 follow-up)"},
	"SoftDeleteField":     {gap, "", "changes generated query predicates; not DDL, but a silent behaviour change (task #12 follow-up)"},
	"PartitionField":      {diffed, "diffPartition", ""},
	"TimeField":           {gap, "", "hypertable time column; embedded strips Timescale DDL anyway, so a change is doubly invisible (task #12 follow-up)"},
	"ChunkTimeIntervalMS": {diffed, "diffChunkTimeInterval", ""},
	"TtlField":            {gap, "", "drives the TTL sweeper, whose DELETE is MySQL syntax and silently deletes nothing (task #12 follow-up)"},
	// Not a gap: a gap forces `unverified`, which for a clause with no runtime
	// effect degrades every verdict and buys no safety. has_many / has_one /
	// via parse, validate and diff-walk while emitting nothing; task #14
	// removes them from the grammar, and this entry with them.
	"Relations": {notSchema, "", "has_many/has_one/via parse and emit nothing — no DDL, no generated query; task #14 removes them from the grammar"},
	// Emits no DDL, and is not inert: proto.go renders it as `reserved N;` in
	// the generated .proto. notSchema because this registry polices the
	// database schema — what a migration changes and a rehearsal proves safe.
	// Wire compatibility needs its own completeness check.
	"RetiredProtoNumbers": {notSchema, "", "wire-stability bookkeeping; emits no DDL and changes no generated query — it is rendered into the .proto, which is a separate completeness question"},
	// Turning it on or off changes which generated artifacts exist, not the
	// table: the DDL for a keyless entity is the DDL for the same entity with
	// a key, minus the PRIMARY KEY clause, which the Primary gap above already
	// covers. Flipping it is add-service or drop-service, and that is the same
	// wire question RetiredProtoNumbers is.
	"Keyless": {notSchema, "", "gates the API emitters, not the DDL — a change adds or removes a generated service, which is a wire question rather than a migration one"},
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
	// Same boundary as RetiredProtoNumbers: no DDL, no query change. A change
	// here is a wire break, which is outside this registry rather than
	// harmless.
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
