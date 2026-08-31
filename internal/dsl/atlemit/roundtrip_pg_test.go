package atlemit_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/atlemit"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// The round trip is the property that matters, and one test covers three
// things that could each be wrong on their own:
//
//	live table → introspect → emit .atl → parse → lower → introspect again
//
// If the emitter drops a modifier, the type mapping renders a spelling the
// parser does not accept, or the `table` override is missing so the second
// introspection looks at a table that does not exist — the second diff is
// non-empty and this fails. Asserting on the emitted TEXT instead would pin
// the formatting and miss all three.

func roundTripPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise .atl generation")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// stubFor is what discovery hands introspection: an entity naming the physical
// table and nothing else. FromPostgres fills in the columns because its emit
// loop runs over live columns the declaration does not name.
func stubFor(namespace, name, table string) *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: name, Namespace: namespace, TableName: table,
	}}}
}

func TestGeneratedSchemaRoundTrips(t *testing.T) {
	pool := roundTripPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS rtrip CASCADE`,
		`CREATE SCHEMA rtrip`,
		// The type list is the point of this fixture, not scenery. The first
		// version used only bigint/varchar(N)/text/numeric/boolean/timestamptz
		// and passed while the emitter produced unparseable .atl for THREE
		// other shapes — arrays, unbounded varchar and double precision. A
		// round-trip test is only as wide as the columns it round-trips.
		`CREATE TABLE rtrip.customer (
			id          bigint PRIMARY KEY,
			email       varchar(255) NOT NULL UNIQUE,
			nickname    text,
			balance     numeric(12, 2) NOT NULL DEFAULT 0,
			active      boolean NOT NULL DEFAULT true,
			created_at  timestamptz NOT NULL DEFAULT now(),
			label       varchar(40) DEFAULT 'none',
			tags        text[],
			lat         double precision,
			unbounded   varchar,
			ref         uuid,
			payload     jsonb,

			-- Every type added to the registry that can sit in a column, for
			-- the reason the comment above gives. A width dropped on the way
			-- out -- char(6) emitted as bare char -- reads back as CHAR(1) and
			-- reports a type change against the column it came from.
			code        char(6),
			seen_at     timestamp,
			wallclock   time,
			zoned       timetz,
			doc         json,
			markup      xml,
			price       money,
			ident       name,
			q           tsquery,
			mac         macaddr,
			mac8        macaddr8,
			addr        inet,
			net         cidr,
			flags       bit(8),
			bits        varbit,
			search      tsvector,
			span        int4range,
			days        daterange,
			spot        point,
			area        box,

			-- Columns named after field modifiers. The emitted line puts the
			-- name at member indent, which is what separates it from a
			-- modifier of the field above; a name that emits but does not read
			-- back fails here rather than at the customer's next plan.
			identity    double precision,
			"default"   text
		)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS rtrip CASCADE`)
	})

	const physical = "rtrip.customer"

	// 1. Introspect a stub into a populated entity.
	first, _, _, err := introspect.FromPostgres(ctx, pool, stubFor("rtrip", "Customer", physical))
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n := len(first.Entities[0].Fields); n != 34 {
		t.Fatalf("introspection produced %d fields, want 34 — the stub did not get "+
			"filled in, so the rest of this test proves nothing", n)
	}

	// 2. Emit .atl.
	src := atlemit.Entity(&first.Entities[0], physical)
	t.Logf("generated:\n%s", src)

	// 3. Parse and lower it — the first thing a customer's `tide plan` does.
	f, err := dsl.Parse("generated.atl", []byte(src))
	if err != nil {
		t.Fatalf("generated .atl does not parse: %v\n%s", err, src)
	}
	lowered, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("generated .atl does not lower: %v\n%s", err, src)
	}

	// 4. Emit protobuf from it. A declaration that parses and lowers can still
	//    be unusable: schema.SQLType renders several Postgres types that
	//    coltype has no proto mapping for, so plan and apply run clean and
	//    `tide codegen` is the first thing to fail — after the customer has
	//    committed the file and migrated the database. Checking it here makes
	//    "usable" mean usable through codegen.
	if _, err := codegen.EmitProto(lowered); err != nil {
		t.Fatalf("generated .atl does not survive codegen: %v\n%s", err, src)
	}

	// 5. Introspect the same table against the generated declaration. Any
	//    difference means the generated file describes a table other than the
	//    one it was generated from.
	second, _, _, err := introspect.FromPostgres(ctx, pool, lowered)
	if err != nil {
		t.Fatalf("introspect generated: %v", err)
	}
	codegen.AssignProtoNumbers(second, lowered)
	d := codegen.ComputeDiff(second, lowered)
	if !d.IsEmpty() {
		for _, c := range d.All() {
			t.Errorf("round trip lost %s/%s: %s (%s)", c.EntityID, c.Field, c.Detail, c.Kind)
		}
		t.Fatalf("the generated declaration does not describe the table it came from\n%s", src)
	}
}

// TestGeneratedSchemaOmitsWhatCodegenCannotCarry is the other half of the
// round trip: what happens to a column atlantis cannot describe.
//
// A legacy database is full of them. The property is total: every column is
// either declared, or named in the NOT DECLARED block — none vanishes, because
// a declaration that silently omits a column reads as complete.
//
// The fixture mixes types the registry carries with one it does not, so the
// test keeps working as types are added: `inet` moves from the second group to
// the first by being registered, and nothing here has to be edited except this
// comment.
//
// A column that renders valid Postgres but has no proto mapping is the case
// that matters. Plan and apply are clean and the checkpoint is written;
// `tide codegen` is the first step to fail.
func TestGeneratedSchemaOmitsWhatCodegenCannotCarry(t *testing.T) {
	pool := roundTripPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS rtripgap CASCADE`,
		`CREATE SCHEMA rtripgap`,
		`CREATE TABLE rtripgap.legacy (
			id         bigint PRIMARY KEY,
			kept       text NOT NULL,
			created    timestamp NOT NULL,
			blob       json,
			addr       inet,
			code       char(4),
			at_time    time,
			rel        oid
		)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS rtripgap CASCADE`)
	})

	ir, _, _, err := introspect.FromPostgres(ctx, pool, stubFor("rtripgap", "Legacy", "rtripgap.legacy"))
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n := len(ir.Entities[0].Fields); n != 8 {
		t.Fatalf("introspection produced %d fields, want 8 — the fixture is not "+
			"exercising what this test claims", n)
	}

	src := atlemit.Entity(&ir.Entities[0], "rtripgap.legacy")
	t.Logf("generated:\n%s", src)

	f, err := dsl.Parse("generated.atl", []byte(src))
	if err != nil {
		t.Fatalf("generated .atl does not parse: %v\n%s", err, src)
	}
	lowered, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("generated .atl does not lower: %v\n%s", err, src)
	}
	// The property that failed before: parse and lower were never the binding
	// constraint, codegen was.
	if _, err := codegen.EmitProto(lowered); err != nil {
		t.Fatalf("generated .atl does not survive codegen: %v\n%s", err, src)
	}

	declared := map[string]bool{}
	for _, fl := range lowered.Entities[0].Fields {
		declared[fl.Name] = true
	}

	// Total over the fixture: declared, or named in the file. EmitProto above
	// already established that everything declared survives codegen, so a
	// column in neither group is one that vanished.
	for _, name := range []string{"id", "kept", "created", "blob", "addr", "code", "at_time", "rel"} {
		if declared[name] {
			continue
		}
		if !strings.Contains(src, name) {
			t.Errorf("column %q was neither declared nor named in the "+
				"NOT DECLARED block, so the declaration reads as complete "+
				"while the column is missing:\n%s", name, src)
		}
	}

	// The columns the registry carries are declared rather than parked.
	for _, name := range []string{"id", "kept", "created", "blob", "addr", "code", "at_time"} {
		if !declared[name] {
			t.Errorf("column %q has a registered type and was left out anyway:\n%s", name, src)
		}
	}

	// oid has no .atl spelling. pgx reads oid columns in its own type
	// resolution, so it is the one type excluded from the text codecs rather
	// than carried by them, and a column of it is parked.
	if declared["rel"] {
		t.Errorf("rel was declared, but codegen has no mapping for oid — the "+
			"file passes plan and apply and then fails at `tide codegen`:\n%s", src)
	}
}

// TestGeneratedSchemaInventsNoIsolation is the same defect class as 3f819ac,
// checked on the generation side.
//
// A table with no row-level security must generate no `partition by`. Getting
// this wrong would hand a customer a file claiming tenant isolation their
// database does not enforce — and because they committed it, every later plan
// would agree with it.
func TestGeneratedSchemaInventsNoIsolation(t *testing.T) {
	pool := roundTripPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS rtripiso CASCADE`,
		`CREATE SCHEMA rtripiso`,
		// A tenant column by name and shape, with no policy behind it. The
		// tempting wrong behaviour is to infer isolation from the name.
		`CREATE TABLE rtripiso.doc (
			id     bigint PRIMARY KEY,
			tenant varchar(16) NOT NULL,
			body   text
		)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS rtripiso CASCADE`)
	})

	ir, _, _, err := introspect.FromPostgres(ctx, pool, stubFor("rtripiso", "Doc", "rtripiso.doc"))
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if got := ir.Entities[0].PartitionField; got != "" {
		t.Fatalf("introspection reported partition_field = %q on a table with no "+
			"policy", got)
	}
	src := atlemit.Entity(&ir.Entities[0], "rtripiso.doc")
	if strings.Contains(src, "partition by") {
		t.Errorf("generated .atl declares tenant isolation the database does not "+
			"enforce:\n%s", src)
	}
}
