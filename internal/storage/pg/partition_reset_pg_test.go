package pg

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// A pooled connection must never be handed out with a tenant already bound.
//
// atlantis binds atlantis.tenant transaction-locally, so its own binds revert
// on their own. A value can still arrive from outside this process, and the
// role default used below is the easiest of those to reproduce: an operator
// runs ALTER ROLE ... SET for some unrelated reason, every new backend starts
// with that value, and the first request on each connection that does not bind
// reads that tenant's rows instead of nothing.
//
// Clear at connect rather than at release, and the difference was measured
// before it was chosen. A reset per release costs a round trip on every
// request — 92,821 tps against 46,648 on 8 connections — which is more than
// `partition by` costs in total. Once per physical connection amortises to
// nothing.
//
// # The role is created here rather than borrowed
//
// The first version of this test ran ALTER ROLE against the role the DSN
// already names, which is the role every other package's tests connect as. A
// role default is cluster-wide and takes effect on the next connection, so for
// as long as this test ran, every concurrent package inherited
// atlantis.tenant and every set_partition call in the repository failed with
// "partition already set". Five tests across three packages went red, none of
// them for their own reasons. A test that mutates shared server state has to
// bring its own subject.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/storage/pg/ -run TenantResetAtConnect -v
func TestTenantResetAtConnect(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the connect-time clear")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup, not defer. Deferred closes run when the test function
	// returns, which is BEFORE any t.Cleanup registered later — so a cleanup
	// that undoes server state through this connection would run against a
	// closed one. That happened: the first version of this test reset its role
	// default through a connection its own defer had already closed, the error
	// was discarded, and the default stayed on the shared role. Every
	// set_partition call in the repository failed with "partition already set"
	// until it was cleared by hand. Cleanups run last-registered-first, so this
	// one runs after the state cleanup below.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	var haveFn bool
	if err := admin.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'atlantis' AND p.proname = 'current_partition')`).Scan(&haveFn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !haveFn {
		t.Skip("partition migrations not applied to this database")
	}

	// Catalog writes — CREATE, GRANT and both DROPs — go through pgcatalog.
	//
	// The first version took the lock around CREATE ROLE only, released it with
	// a defer, and dropped the role from t.Cleanup. Go runs a test function's
	// defers BEFORE its cleanups, so the unlock happened first and two of the
	// three catalog writes ran unprotected — in a test whose own comment named
	// DROP ROLE as the hazard. The lock also lived in this package and could not
	// be named by the other three that create roles.
	cleanup := func() {
		pgcatalog.Exec(t, dsn,
			`DROP OWNED BY tenantreset_probe`,
			`DROP ROLE IF EXISTS tenantreset_probe`)
	}
	cleanup()
	t.Cleanup(cleanup)

	// The role default lands on a role nothing else connects as.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE tenantreset_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO tenantreset_probe;
ALTER ROLE tenantreset_probe SET "atlantis.tenant" = 'sticky-tenant';`)
		return err
	})

	probeDSN := withUserInfo(dsn, "tenantreset_probe", "probe")

	// Proves the fixture bites. Without an inherited value this test could not
	// tell a pool that clears from a pool that does nothing.
	bare, err := pgx.Connect(ctx, probeDSN)
	if err != nil {
		t.Fatalf("connect bare: %v", err)
	}
	var inherited *string
	err = bare.QueryRow(ctx, `SELECT atlantis.current_partition()`).Scan(&inherited)
	_ = bare.Close(ctx)
	if err != nil {
		t.Fatalf("read discriminator on a bare connection: %v", err)
	}
	if inherited == nil || *inherited != "sticky-tenant" {
		t.Fatalf("the fixture did not take: a connection opened outside the pool "+
			"sees %v, want \"sticky-tenant\"", inherited)
	}

	cfg := DefaultConfig(probeDSN)
	cfg.MaxConns = 2
	cfg.MinConns = 1
	pool, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	// Every connection the pool opens, not just the first: MinConns warms one
	// and the second is opened on demand, and a clear wired into only one of
	// those paths is the same bug with a narrower window.
	for i := 0; i < 4; i++ {
		var got *string
		if err := pool.Raw().QueryRow(ctx,
			`SELECT atlantis.current_partition()`).Scan(&got); err != nil {
			t.Fatalf("read discriminator from pool: %v", err)
		}
		if got != nil {
			t.Fatalf("a pooled connection was handed out with tenant %q already "+
				"bound. The first request on it that does not bind reads that "+
				"tenant's rows rather than nothing", *got)
		}
	}
}

// withUserInfo swaps the credentials in a libpq URL, keeping everything else.
//
// By parsing rather than by substring replacement: a Replace on a hardcoded
// "user:pass@" silently returns the DSN unchanged for any other spelling, and
// the caller then connects as the original role while believing it is the new
// one.
func withUserInfo(dsn, user, pass string) string {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}
