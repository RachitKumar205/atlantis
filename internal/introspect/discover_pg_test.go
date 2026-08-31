package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// DiscoverTables names tables nothing has declared. All of it is exclusion, and
// each exclusion can drop a table that needed listing.
//
// A missing table is invisible: nothing downstream reports "40 of 41 tables",
// so the fortieth is never declared and surfaces when a query reaches a table
// atlantis does not know. Each exclusion is asserted separately rather than
// inferred from one clean run.

func discoverPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise table discovery")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestDiscoverTables(t *testing.T) {
	pool := discoverPool(t)
	ctx := context.Background()

	// A private schema so this test cannot be perturbed by whatever else the
	// shared database holds, and cannot perturb it.
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS disco CASCADE`,
		`CREATE SCHEMA disco`,
		`CREATE TABLE disco.plain (id bigint primary key, note text)`,
		`CREATE TABLE disco.declared_already (id bigint primary key)`,
		// A partitioned parent with two children. The children are ordinary
		// relations in pg_class; discovering them would offer the customer
		// one entity per partition, none separately declarable.
		`CREATE TABLE disco.events (id bigint, at timestamptz not null) PARTITION BY RANGE (at)`,
		`CREATE TABLE disco.events_2026 PARTITION OF disco.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE disco.events_2027 PARTITION OF disco.events
			FOR VALUES FROM ('2027-01-01') TO ('2028-01-01')`,
		// The older INHERITS mechanism. Its children are partitions in every
		// sense that matters here, and relispartition is false for them —
		// which is how a filter written for declarative partitioning lets
		// them through.
		`CREATE TABLE disco.legacy_parent (id bigint, tag text)`,
		`CREATE TABLE disco.legacy_child () INHERITS (disco.legacy_parent)`,
		`CREATE VIEW disco.a_view AS SELECT id FROM disco.plain`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS disco CASCADE`)
	})

	// One table is already declared, with a table override pointing at it.
	declared := &dsl.IR{Entities: []dsl.Entity{{
		Name: "DeclaredAlready", Namespace: "disco", TableName: "disco.declared_already",
	}}}

	got, err := DiscoverTables(ctx, pool, declared, []string{"disco"})
	if err != nil {
		t.Fatalf("DiscoverTables: %v", err)
	}
	found := map[string]bool{}
	for _, d := range got {
		found[d.Qualified()] = true
	}

	for _, tc := range []struct {
		name  string
		table string
		want  bool
		why   string
	}{
		{"an undeclared table", "disco.plain", true, "this is the whole point"},
		{"a declared table", "disco.declared_already", false,
			"already described; offering it again would propose a duplicate entity"},
		{"a partitioned parent", "disco.events", true,
			"the parent is the thing a customer declares"},
		{"a partition child", "disco.events_2026", false,
			"children are storage, not schema — a time-partitioned table would " +
				"otherwise contribute one bogus entity per partition"},
		{"another partition child", "disco.events_2027", false, "same"},
		{"an inheritance parent", "disco.legacy_parent", true,
			"the parent is the thing a customer declares, whichever mechanism " +
				"puts rows under it"},
		{"an inheritance child", "disco.legacy_child", false,
			"storage, not schema — and relispartition is false for it, so the " +
				"declarative filter alone does not see it. 112 of these arrived " +
				"as skipped tables from one public dataset"},
		{"a view", "disco.a_view", false,
			"relkind filters to tables; a view has no schema atlantis can own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if found[tc.table] != tc.want {
				t.Errorf("discovered(%s) = %v, want %v — %s", tc.table, found[tc.table], tc.want, tc.why)
			}
		})
	}
}

// atlantis.* holds the job queue, the IR checkpoint and caller registrations.
// Discovering them proposes atlantis's own bookkeeping as the caller's schema.
func TestDiscoverTablesExcludesAtlantisOwnMachinery(t *testing.T) {
	pool := discoverPool(t)
	ctx := context.Background()

	// The precondition is that there is something to exclude. Asserted on the
	// CATALOGUE, not on the result: an earlier version skipped when discovery
	// returned nothing, which is precisely the state a broken-but-silent
	// exclusion produces — and on a database whose only public tables are
	// atlantis's own migration bookkeeping, that is the normal state. The test
	// would have passed by never running.
	var owned int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f') AND n.nspname = 'atlantis'`).Scan(&owned); err != nil {
		t.Fatalf("count atlantis tables: %v", err)
	}
	if owned == 0 {
		t.Fatal("the atlantis schema holds no tables, so this database cannot " +
			"demonstrate the exclusion — has it been migrated?")
	}

	// nil schemas means "every non-system schema", which is what onboarding
	// uses and the only mode in which this exclusion is reachable.
	got, err := DiscoverTables(ctx, pool, nil, nil)
	if err != nil {
		t.Fatalf("DiscoverTables: %v", err)
	}
	t.Logf("%d tables in atlantis.*, %d discovered elsewhere", owned, len(got))
	for _, d := range got {
		if d.Schema == "atlantis" {
			t.Errorf("discovered %s — that is atlantis's own machinery, not the "+
				"customer's schema", d.Qualified())
		}
		if d.Schema == "public" && atlantisOwnedInPublic[d.Table] {
			t.Errorf("discovered %s — atlantis's migration bookkeeping", d.Qualified())
		}
		if d.Schema == "information_schema" || d.Schema == "pg_catalog" {
			t.Errorf("discovered %s — system catalogue", d.Qualified())
		}
	}
}

// A partition child pointed at explicitly is still read.
//
// Discovery refuses to offer children as entities; introspection must still
// fill one in when a caller has declared it with `table "schema.child"`. The
// two use different queries for that reason, and a filter added to the second
// would make such a declaration unreadable while leaving it parseable.
func TestAnExplicitlyDeclaredPartitionChildIsStillRead(t *testing.T) {
	pool := discoverPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS discokid CASCADE`,
		`CREATE SCHEMA discokid`,
		`CREATE TABLE discokid.events (id bigint, at timestamptz not null) PARTITION BY RANGE (at)`,
		`CREATE TABLE discokid.events_2026 PARTITION OF discokid.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE discokid.legacy_parent (id bigint, tag text)`,
		`CREATE TABLE discokid.legacy_child () INHERITS (discokid.legacy_parent)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS discokid CASCADE`)
	})

	for _, tc := range []struct{ name, table string }{
		{"a declarative partition child", "discokid.events_2026"},
		{"an inheritance child", "discokid.legacy_child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared := &dsl.IR{Entities: []dsl.Entity{{
				Name: "Child", Namespace: "discokid", TableName: tc.table,
				Fields: []dsl.Field{
					{Name: "id", Type: dsl.FieldType{Name: "bigint"}},
				},
			}}}
			live, _, _, err := FromPostgres(ctx, pool, declared)
			if err != nil {
				t.Fatalf("introspect %s: %v", tc.table, err)
			}
			if len(live.Entities) != 1 {
				t.Fatalf("got %d entities, want 1", len(live.Entities))
			}
			if f := live.Entities[0].FindField("id"); f == nil {
				t.Errorf("%s was declared and read back with no id column, so the "+
					"declaration cannot be applied against it", tc.table)
			}
		})
	}
}

// SchemasWithTables names a schema only if asking for it would yield tables.
//
// Its exclusions have to match DiscoverTables' or the two disagree, and the
// disagreement is silent: a schema holding nothing but partition children is
// advertised as holding tables, and the read that follows returns none. The
// message it feeds — "this database keeps its tables in ..." — then names the
// one schema that cannot help.
//
// A child may live in a different schema from its parent, which is what makes
// this reachable rather than theoretical.
func TestSchemasWithTablesMatchesWhatDiscoverTablesReturns(t *testing.T) {
	pool := discoverPool(t)
	ctx := context.Background()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS censuskids CASCADE`,
		`DROP SCHEMA IF EXISTS censusparent CASCADE`,
		`CREATE SCHEMA censusparent`,
		`CREATE SCHEMA censuskids`,
		`CREATE TABLE censusparent.events (id bigint, at timestamptz not null) PARTITION BY RANGE (at)`,
		`CREATE TABLE censuskids.events_2026 PARTITION OF censusparent.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		`CREATE TABLE censusparent.legacy (id bigint)`,
		`CREATE TABLE censuskids.legacy_child () INHERITS (censusparent.legacy)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DROP SCHEMA IF EXISTS censuskids CASCADE`)
		_, _ = pool.Exec(c, `DROP SCHEMA IF EXISTS censusparent CASCADE`)
	})

	named, err := SchemasWithTables(ctx, pool)
	if err != nil {
		t.Fatalf("SchemasWithTables: %v", err)
	}
	in := map[string]bool{}
	for _, s := range named {
		in[s] = true
	}
	if !in["censusparent"] {
		t.Errorf("censusparent holds two tables and was not named")
	}
	if in["censuskids"] {
		t.Errorf("censuskids holds only partition children and was named, so the " +
			"message points at a schema a read of it returns nothing from")
	}

	// The contract, stated directly: every schema named yields tables.
	for _, s := range named {
		got, err := DiscoverTables(ctx, pool, nil, []string{s})
		if err != nil {
			t.Fatalf("DiscoverTables(%s): %v", s, err)
		}
		if len(got) == 0 {
			t.Errorf("SchemasWithTables named %q and DiscoverTables returns nothing for it", s)
		}
	}
}
