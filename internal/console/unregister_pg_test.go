package console

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/migrations"
)

// Removing everything the console holds for a destroyed organisation.
//
// # Why this needs a real database
//
// The whole function is four DELETEs and a decision about which tables they run
// against. There is nothing to fake: the interesting question is whether the
// statements match the schema, and whether the one table that must survive
// actually survives. A stub would answer neither.
//
// The console schema declares no foreign keys at all, so nothing cascades and
// every table has to be named. That is exactly the shape where a fix removes the
// row somebody noticed and leaves the three they did not.

func unregisterDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	adminDSN := requireTestPG(t)
	const dbName = "atlantis_console_unregister"
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, dbName)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.RunFS(dsn, migrations.Console, "console",
		migrate.ConsoleHistoryTable, quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// audit_log is range-partitioned by created_at and the migrations create no
	// partitions — the console does that at startup, which this fixture does not
	// run. Without one, inserting an audit row fails with "no partition of
	// relation found" and the test that proves the audit log SURVIVES would
	// never get a row to survive.
	//
	// Bounded by month rather than named for one, so this does not stop working
	// in September.
	if _, err := pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS console.audit_log_unregister_test
		PARTITION OF console.audit_log
		FOR VALUES FROM (date_trunc('month', NOW()))
		            TO (date_trunc('month', NOW()) + interval '1 month')`); err != nil {
		t.Fatalf("create audit partition: %v", err)
	}
	return dsn, pool
}

// seedOrgEverywhere puts one organisation into every table that references one.
func seedOrgEverywhere(t *testing.T, pool *pgxpool.Pool, org string) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO console.orgs (org, atl_endpoint, atl_health_addr, ca_pem,
	        client_cert_pem, client_key_ct, updated_at)
	      VALUES ($1, 'h:1', 'h:2', 'ca', 'cert', '\x00'::bytea, NOW())`, org)
	exec(`INSERT INTO console.sessions (token, expires_at, subject, org, role, email)
	      VALUES ($1, NOW() + interval '1 hour', 'sub', $2, 'admin', 'e@example.test')`,
		org+"-session", org)
	exec(`INSERT INTO console.enroll_tokens (token_sha256, org, caller, created_by, expires_at)
	      VALUES ($1, $2, 'backend', 'e@example.test', NOW() + interval '1 hour')`,
		[]byte(org+"-token"), org)
	exec(`INSERT INTO console.caller_certs (fingerprint, org, caller, expires_at)
	      VALUES ($1, $2, 'backend', NOW() + interval '1 hour')`,
		[]byte(org+"-fingerprint"), org)
	exec(`INSERT INTO console.audit_log (org, actor, action)
	      VALUES ($1, 'e@example.test', 'org.something')`, org)
}

func countFor(t *testing.T, pool *pgxpool.Pool, table, org string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM console.`+table+` WHERE org = $1`, org).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// Everything operational goes; the audit log stays.
func TestUnregisterRemovesTheOrganisationButKeepsTheAuditLog(t *testing.T) {
	dsn, pool := unregisterDB(t)
	const org = "gone"
	seedOrgEverywhere(t, pool, org)

	// Proven present first. Without this the assertions below pass against a
	// seed that silently inserted nothing.
	for _, table := range []string{"orgs", "sessions", "enroll_tokens", "caller_certs", "audit_log"} {
		if countFor(t, pool, table, org) == 0 {
			t.Fatalf("the fixture put nothing in console.%s", table)
		}
	}

	if err := UnregisterOrg(context.Background(), dsn, org); err != nil {
		t.Fatalf("UnregisterOrg: %v", err)
	}

	for _, table := range []string{"orgs", "sessions", "enroll_tokens", "caller_certs"} {
		if n := countFor(t, pool, table, org); n != 0 {
			t.Errorf("console.%s still holds %d row(s) for a destroyed organisation", table, n)
		}
	}
	// The one that must survive. The audit log answers "what happened to acme"
	// and has to keep answering after acme stops existing — the same reason the
	// purge writes an audit row on its way out.
	if n := countFor(t, pool, "audit_log", org); n == 0 {
		t.Error("the audit log was removed with the organisation; there is now no " +
			"record that it ever existed or what was done to it")
	}
}

// Another organisation is untouched.
//
// Every statement is scoped by org, and a missing WHERE would empty the table
// for the whole fleet — which no test asserting only "gone is gone" would catch.
func TestUnregisterLeavesOtherOrganisationsAlone(t *testing.T) {
	dsn, pool := unregisterDB(t)
	seedOrgEverywhere(t, pool, "gone")
	seedOrgEverywhere(t, pool, "staying")

	if err := UnregisterOrg(context.Background(), dsn, "gone"); err != nil {
		t.Fatalf("UnregisterOrg: %v", err)
	}

	for _, table := range []string{"orgs", "sessions", "enroll_tokens", "caller_certs", "audit_log"} {
		if n := countFor(t, pool, table, "staying"); n == 0 {
			t.Errorf("console.%s lost the surviving organisation's rows; the delete "+
				"is not scoped to one organisation", table)
		}
	}
}

// Removing an organisation that was never registered is success.
//
// The purge path retries after a partial failure, so a second pass finding the
// rows already gone is the ordinary case. Reporting it as an error would make a
// converging retry look like a stuck one.
func TestUnregisterIsIdempotent(t *testing.T) {
	dsn, pool := unregisterDB(t)
	seedOrgEverywhere(t, pool, "gone")

	for i := range 2 {
		if err := UnregisterOrg(context.Background(), dsn, "gone"); err != nil {
			t.Fatalf("UnregisterOrg call %d: %v", i+1, err)
		}
	}
	if err := UnregisterOrg(context.Background(), dsn, "never-existed"); err != nil {
		t.Errorf("removing an organisation that was never registered failed: %v", err)
	}
}

func TestUnregisterRefusesAnEmptyOrganisation(t *testing.T) {
	dsn, _ := unregisterDB(t)
	// An empty name would match no row and report success, so the caller would
	// believe a registration had been removed. It is also the shape a bug in the
	// purge path would take.
	if err := UnregisterOrg(context.Background(), dsn, ""); err == nil {
		t.Error("an empty organisation name was accepted")
	}
}

// An unreachable database is reported as a connection failure.
//
// pgxpool.NewWithConfig is lazy, so without the Ping this surfaces as a failure
// to delete rows — which reads as a schema problem rather than an address one.
func TestUnregisterReportsAnUnreachableDatabase(t *testing.T) {
	requireTestPG(t)
	const bad = "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=2"
	err := UnregisterOrg(context.Background(), bad, "gone")
	if err == nil {
		t.Fatal("UnregisterOrg succeeded against an unreachable database")
	}
	if got := err.Error(); !contains(got, "connect") {
		t.Errorf("the error does not report a connection problem: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
