package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// DiscoverTables is the capability adopt never had: naming tables nobody has
// declared. Everything it does is exclusion, and every exclusion is a chance
// to drop a table the customer needed to see.
//
// A missing table is invisible. Nothing downstream reports "we found 40 of your
// 41 tables" — the customer adopts, the fortieth is never declared, and they
// discover it when something queries a table atlantis does not know about. So
// each exclusion is asserted individually rather than trusting one clean run.

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

// TestDiscoverTablesExcludesAtlantisOwnMachinery covers the exclusion that
// would be most embarrassing to get wrong.
//
// atlantis.* holds the job queue, the IR checkpoint and caller registrations.
// Discovering them would open onboarding by proposing that the customer adopt
// our bookkeeping as their schema.
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
