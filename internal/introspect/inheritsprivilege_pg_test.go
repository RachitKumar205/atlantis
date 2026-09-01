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

// Discovery works against a connection that may not read pg_inherits.
//
// It is readable by PUBLIC in a default installation, so a test running as the
// owner never sees otherwise. EBI's public mirror revokes it, and reading it
// unconditionally failed the whole import with `permission denied for table
// pg_inherits` — every table, not just the partitions the read was for.
//
// The fallback is narrower, not absent: relispartition lives in pg_class,
// which is always readable, and covers declarative partitioning. A child
// attached with INHERITS is discovered as a table of its own, which is what
// happened before pg_inherits was consulted at all.
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

	if introspect.InheritsReadable(ctx, rp) {
		t.Fatal("the restricted role can still read pg_inherits, so this test proves nothing")
	}

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
	// Named rather than asserted absent: without pg_inherits there is no way
	// to know this is a child, and discovering it is the documented cost.
	if !found["noinh.child"] {
		t.Log("the INHERITS child was excluded, so the privilege is not actually revoked")
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
