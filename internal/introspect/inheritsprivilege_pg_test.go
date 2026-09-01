package introspect_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// Discovery is complete against a connection that may not read pg_inherits.
//
// It is readable by PUBLIC in a default installation, so a test running as the
// owner never sees otherwise. EBI's public mirror revokes it, and reading it
// unconditionally failed the whole import with `permission denied for table
// pg_inherits` — every table, not just the partitions the read was for.
//
// Answering with relispartition instead was worse than the failure: it covers
// declarative partitioning only, so 112 INHERITS children came back, and
// keyless entities meant they were declared rather than skipped. The import
// went from 84 entities to 209 and the plan over them timed out.
//
// So the child test reads pg_attribute, which introspection cannot run without
// anyway, and both mechanisms are excluded at either privilege level.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/introspect -run InheritsPrivilege -count=1
func TestDiscoveryWithoutInheritsPrivilege(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the privilege fallback")
	}
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(owner.Close)

	const role = "atlantis_no_inherits"
	drop := func() {
		c := context.Background()
		_, _ = owner.Exec(c, `DROP SCHEMA IF EXISTS noinh CASCADE`)
		_, _ = owner.Exec(c, fmt.Sprintf(`DROP OWNED BY %s`, role))
		_, _ = owner.Exec(c, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, role))
	}
	drop()
	t.Cleanup(drop)

	for _, stmt := range []string{
		`CREATE SCHEMA noinh`,
		`CREATE TABLE noinh.parent (id bigint, tag text)`,
		`CREATE TABLE noinh.child () INHERITS (noinh.parent)`,
		`CREATE TABLE noinh.events (id bigint, at timestamptz not null) PARTITION BY RANGE (at)`,
		`CREATE TABLE noinh.events_2026 PARTITION OF noinh.events
			FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'x'`, role),
		fmt.Sprintf(`GRANT USAGE ON SCHEMA noinh TO %s`, role),
		// What a hardened public database does.
		fmt.Sprintf(`REVOKE SELECT ON pg_catalog.pg_inherits FROM PUBLIC, %s`, role),
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	// Restore the default for everything else that uses this database.
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `GRANT SELECT ON pg_catalog.pg_inherits TO PUBLIC`)
	})

	restricted := strings.Replace(dsn, "atlantis:atlantis@", role+":x@", 1)
	rp, err := pgxpool.New(ctx, restricted)
	if err != nil {
		t.Fatalf("restricted pool: %v", err)
	}
	t.Cleanup(rp.Close)

	// The whole point: discovery returns tables rather than failing.
	got, err := introspect.DiscoverTables(ctx, rp, &dsl.IR{}, []string{"noinh"})
	if err != nil {
		t.Fatalf("discovery failed for a role that cannot read pg_inherits: %v", err)
	}
	found := map[string]bool{}
	for _, d := range got {
		found[d.Qualified()] = true
	}
	if !found["noinh.parent"] || !found["noinh.events"] {
		t.Errorf("a parent was lost: %v", found)
	}
	if found["noinh.events_2026"] {
		t.Error("a declarative partition child was discovered; relispartition covers those " +
			"and is readable without pg_inherits")
	}
	// The point of the change: an INHERITS child is excluded even here,
	// because identifying one reads pg_attribute rather than pg_inherits.
	// Falling back to relispartition re-admitted 112 of these from one public
	// dataset, and keyless entities then declared every one of them.
	if found["noinh.child"] {
		t.Error("an INHERITS child was discovered by a role that cannot read " +
			"pg_inherits; the child test must not depend on that privilege")
	}

	// And the owner still gets the complete answer.
	full, err := introspect.DiscoverTables(ctx, owner, &dsl.IR{}, []string{"noinh"})
	if err != nil {
		t.Fatalf("discovery as owner: %v", err)
	}
	for _, d := range full {
		if d.Qualified() == "noinh.child" {
			t.Error("the owner can read pg_inherits and the INHERITS child was still discovered")
		}
	}
}
