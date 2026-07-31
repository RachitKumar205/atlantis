package codegen

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// chunk_time_interval was documented three times in the grammar reference and
// implemented zero times — not parsed, not lowered, not emitted. It is
// implemented rather than removed because the surrounding feature is live:
// sql.go already emits create_hypertable, and three schemas in the corpus
// declare hypertables.
//
// Licensing is load-bearing here and was checked, not assumed. create_hypertable
// and set_chunk_time_interval are both defined in timescaledb's sql/ddl_api.sql,
// which carries the Apache 2.0 header. That matters because the Timescale
// License forbids offering TSL software as a database service and its "Value
// Added" exception requires that users be prohibited from modifying schema via
// DDL — exactly what this product exists to permit. See task #33.

func hypertable(intervalMS int) *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Reading", Namespace: "iot", Kind: dsl.EntityKindHypertable,
		TimeField: "recorded_at",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "recorded_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true},
		},
		ChunkTimeIntervalMS: intervalMS,
	}}}
}

func TestChunkTimeIntervalReachesCreateHypertable(t *testing.T) {
	const sevenDays = 7 * 24 * 60 * 60 * 1000

	with, err := EmitInitial(hypertable(sevenDays))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if !strings.Contains(with.Up, "chunk_time_interval =>") {
		t.Errorf("the declared interval never reached create_hypertable:\n%s", with.Up)
	}

	// Passed as an INTERVAL literal, not a bare integer. The parameter is
	// ANYELEMENT, and an integer is read in the time column's own units —
	// microseconds for timestamptz — so 604800000 meaning "7 days" would
	// silently become 10 minutes. This assertion is the difference between a
	// correct chunk size and one off by four orders of magnitude.
	if !strings.Contains(with.Up, "INTERVAL '604800000 milliseconds'") {
		t.Errorf("interval not passed as an INTERVAL literal; a bare integer is "+
			"interpreted in the time column's units:\n%s", with.Up)
	}

	// Unset must emit no argument at all, so TimescaleDB's own default applies
	// rather than a number of ours that would then need keeping current.
	without, err := EmitInitial(hypertable(0))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if strings.Contains(without.Up, "chunk_time_interval") {
		t.Errorf("unset interval still emitted an argument:\n%s", without.Up)
	}
	if !strings.Contains(without.Up, "create_hypertable(") {
		t.Errorf("the hypertable itself stopped being created:\n%s", without.Up)
	}
}

// Changing the interval must emit set_chunk_time_interval. create_hypertable's
// argument applies only at creation and the call carries if_not_exists, so
// re-running it against an existing hypertable changes nothing — the clause
// would work on a fresh table and silently no-op on every edit afterwards.
func TestChangingChunkTimeIntervalIsNotASilentNoOp(t *testing.T) {
	from := hypertable(7 * 24 * 60 * 60 * 1000)
	to := hypertable(24 * 60 * 60 * 1000)

	d := ComputeDiff(from, to)
	all := append(append([]Change{}, d.Additive...), d.BackfillRequired...)
	all = append(all, d.Breaking...)
	if len(all) != 1 || all[0].Kind != KindChunkTimeIntervalChanged {
		t.Fatalf("changing the interval produced %+v, want one "+
			"chunk_time_interval_changed", all)
	}
	// Additive: set_chunk_time_interval affects only chunks created afterwards,
	// so it cannot fail on data or take a disruptive lock.
	if all[0].Class != ClassAdditive {
		t.Errorf("Class = %v, want additive", all[0].Class)
	}

	scripts, err := EmitSQL(from, to, d)
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if !strings.Contains(scripts.Up, "set_chunk_time_interval(") {
		t.Errorf("no set_chunk_time_interval in the up migration, so the change "+
			"would apply to nothing:\n%s", scripts.Up)
	}
	if !strings.Contains(scripts.Up, "INTERVAL '86400000 milliseconds'") {
		t.Errorf("up does not carry the new interval:\n%s", scripts.Up)
	}
	if !strings.Contains(scripts.Down, "INTERVAL '604800000 milliseconds'") {
		t.Errorf("down does not restore the old interval:\n%s", scripts.Down)
	}
}

// Only a hypertable has chunks. Accepting the clause on a plain entity would
// drop it silently at emit time, which is the failure this whole task exists to
// remove — so it is rejected at validation instead.
func TestChunkTimeIntervalIsRejectedOnAPlainEntity(t *testing.T) {
	src := "entity Reading in iot {\n" +
		"  id bigint primary\n" +
		"  recorded_at timestamptz not null\n" +
		"  chunk_time_interval 7d\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = dsl.Lower([]*dsl.File{f})
	if err == nil {
		t.Fatal("accepted chunk_time_interval on a plain entity; it has nowhere to " +
			"go and would be dropped without a word")
	}
	if !strings.Contains(err.Error(), "only valid on a hypertable") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// End to end from source, since every layer between the keyword and the DDL is
// new: token, AST node, member dispatch, lowering, differ, emitter.
func TestChunkTimeIntervalFromSource(t *testing.T) {
	src := "hypertable Reading in iot on recorded_at {\n" +
		"  id bigint primary\n" +
		"  recorded_at timestamptz not null\n" +
		"  chunk_time_interval 7d\n" +
		"}\n"

	f, err := dsl.Parse("t.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if got := ir.Entities[0].ChunkTimeIntervalMS; got != 7*24*60*60*1000 {
		t.Fatalf("ChunkTimeIntervalMS = %d, want 604800000", got)
	}

	scripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if !strings.Contains(scripts.Up, "INTERVAL '604800000 milliseconds'") {
		t.Errorf("the clause did not survive the trip from source to DDL:\n%s", scripts.Up)
	}
}
