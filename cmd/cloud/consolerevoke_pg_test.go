package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/migrations"
)

// `cloud org revoke-console` and its opposite.
//
// The console cannot do either to itself: revoking is the last thing it could
// do, and restoring is unreachable from it once revoked. These two commands are
// the whole of that path, and a fault in either surfaces during an incident.
//
// Restore is the claim under test. caller_capabilities references
// caller_identities ON DELETE CASCADE (0018), so deleting the row destroys every
// grant, and RegisterCaller refuses the reserved CN. Migration 0033 sets
// revoked_at instead of deleting, which is what leaves the grants in place for
// a restore to clear.
//
// Against the real migrations rather than a hand-built table, since the cascade
// is what the property turns on.

// consoleRevokeDSN builds a private database with the real infra schema.
//
// Private rather than the shared atlantis database on purpose: these tests
// revoke the console, and a run that fails between revoke and restore would
// otherwise leave the developer's own console cut off with no obvious cause.
func consoleRevokeDSN(t *testing.T) string {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise `cloud org revoke-console`")
	}
	const dbName = "atlantis_console_revoke"
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, dbName)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.RunFS(dsn, migrations.Infra, "infra",
		migrate.InfraHistoryTable, quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}
	return dsn
}

func consoleRevokePool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func consoleIsActive(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM atlantis.active_callers WHERE caller = 'atlantis-console')`).
		Scan(&ok); err != nil {
		t.Fatalf("query active_callers: %v", err)
	}
	return ok
}

func consoleGrantCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM atlantis.caller_capabilities WHERE caller = 'atlantis-console'`).
		Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

// The migrations really do seed the console. Everything below assumes it.
func TestTheConsoleIsSeededByTheMigrations(t *testing.T) {
	pool := consoleRevokePool(t, consoleRevokeDSN(t))
	if !consoleIsActive(t, pool) {
		t.Fatal("a freshly migrated database has no active atlantis-console; the " +
			"remaining tests would pass for the wrong reason")
	}
	// Not asserted as an exact number. The grants are spread across migrations
	// and a console feature may add one, which is the very reason restore must
	// not rebuild them from a list — pinning the count here would be a copy of
	// that list in a third place.
	if consoleGrantCount(t, pool) == 0 {
		t.Fatal("the console has no capability grants, so 'grants are kept' proves nothing")
	}
}

// Without -yes it must refuse, and refuse without writing.
//
// A command that refused after cutting the console off would be worse than one
// that did neither: the operator reads "refusing" and believes the organisation
// is reachable.
func TestRevokeConsoleRefusesWithoutYes(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	pool := consoleRevokePool(t, dsn)

	err := orgRevokeConsole([]string{"-org", "acme", "-atl-db", dsn}, quietLog())
	if err == nil {
		t.Fatal("revoke-console succeeded without -yes")
	}
	if !strings.Contains(err.Error(), "-yes") {
		t.Errorf("the refusal does not mention -yes, so it does not say how to proceed: %v", err)
	}
	// It has to say what is at stake. "-yes is required" alone does not tell an
	// operator that this is what locks everyone out of the browser.
	if !strings.Contains(err.Error(), "browser") {
		t.Errorf("the refusal does not say the console becomes unreachable: %v", err)
	}
	if !consoleIsActive(t, pool) {
		t.Fatal("the console was revoked despite the refusal")
	}
}

func TestRevokeConsoleRequiresItsArguments(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	if err := orgRevokeConsole([]string{"-yes", "-atl-db", dsn}, quietLog()); err == nil {
		t.Error("revoke-console with no -org succeeded")
	}
	// -atl-db has no default and no environment fallback, deliberately: Cloud
	// does not know this address, so a default would be wrong for every
	// organisation but one.
	if err := orgRevokeConsole([]string{"-org", "acme", "-yes"}, quietLog()); err == nil {
		t.Error("revoke-console with no -atl-db succeeded")
	}
}

// The round trip, which is the whole point.
func TestRevokeConsoleThenRestoreReturnsItIntact(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	pool := consoleRevokePool(t, dsn)
	before := consoleGrantCount(t, pool)

	if err := orgRevokeConsole([]string{"-org", "acme", "-atl-db", dsn, "-yes"}, quietLog()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if consoleIsActive(t, pool) {
		t.Fatal("the console is still in active_callers after being revoked")
	}
	// The claim migration 0033 rests on: nothing was destroyed, so nothing has
	// to be rebuilt. If this drops to zero the cascade is back and restore is
	// returning a console that authenticates and can do nothing.
	if got := consoleGrantCount(t, pool); got != before {
		t.Errorf("revoking changed the grant count from %d to %d", before, got)
	}

	if err := orgRestoreConsole([]string{"-org", "acme", "-atl-db", dsn}, quietLog()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !consoleIsActive(t, pool) {
		t.Error("the console is still cut off after being restored")
	}
	if got := consoleGrantCount(t, pool); got != before {
		t.Errorf("after the round trip the console holds %d grants, want %d", got, before)
	}
}

// Revoking twice is not an error, and says so.
func TestRevokeConsoleIsIdempotent(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	if err := orgRevokeConsole([]string{"-org", "acme", "-atl-db", dsn, "-yes"}, quietLog()); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	out := captureStdout(t, func() {
		if err := orgRevokeConsole([]string{"-org", "acme", "-atl-db", dsn, "-yes"}, quietLog()); err != nil {
			t.Fatalf("second revoke: %v", err)
		}
	})
	if !strings.Contains(out, "already revoked") {
		t.Errorf("revoking twice does not report that it was already revoked: %q", out)
	}
}

// Restoring a console that is not revoked must not claim to have fixed anything.
//
// This is the case an operator reaches when the console is unreachable for some
// other reason — a bad certificate, a wrong endpoint. Printing "restored" there
// sends them away from the actual fault.
func TestRestoreConsoleSaysWhenThereWasNothingToDo(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	out := captureStdout(t, func() {
		if err := orgRestoreConsole([]string{"-org", "acme", "-atl-db", dsn}, quietLog()); err != nil {
			t.Fatalf("restore: %v", err)
		}
	})
	if !strings.Contains(out, "not revoked") {
		t.Errorf("restoring a live console does not say it was not revoked: %q", out)
	}
	if strings.Contains(out, "restored the console") {
		t.Errorf("restoring a live console reported a restore that did not happen: %q", out)
	}
}

// A database with no console identity is reported as such.
//
// Not "revoked", and not a silent success. Reaching this means the DSN points
// somewhere that is not a migrated atlantis database — a Cloud or console
// database, most likely, since an operator holds three URLs and these commands
// want the one Cloud does not store.
func TestConsoleCommandsReportADatabaseWithNoConsoleIdentity(t *testing.T) {
	dsn := consoleRevokeDSN(t)
	pool := consoleRevokePool(t, dsn)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM atlantis.caller_identities WHERE caller = 'atlantis-console'`); err != nil {
		t.Fatalf("remove the console identity: %v", err)
	}

	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"revoke", func() error {
			return orgRevokeConsole([]string{"-org", "acme", "-atl-db", dsn, "-yes"}, quietLog())
		}},
		{"restore", func() error {
			return orgRestoreConsole([]string{"-org", "acme", "-atl-db", dsn}, quietLog())
		}},
	} {
		err := c.run()
		if err == nil {
			t.Errorf("%s succeeded against a database with no console identity", c.name)
			continue
		}
		// Naming the migration is what turns this from "something is wrong" into
		// a next step: either the database is not an atlantis one, or it has not
		// been migrated.
		if !strings.Contains(err.Error(), "0019") {
			t.Errorf("%s does not name the migration that seeds the identity: %v", c.name, err)
		}
	}
}

// An unreachable database is reported against the address, not against the
// console.
//
// pgxpool.New is lazy, so without the Ping a misspelt DSN surfaces as a failure
// to read caller_identities — which reads as a problem with the organisation
// rather than with what was typed.
func TestConsoleCommandsReportAnUnreachableDatabase(t *testing.T) {
	if os.Getenv("ATLANTIS_TEST_PG") == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise `cloud org revoke-console`")
	}
	const bad = "postgres://nobody:nobody@127.0.0.1:1/nothing?sslmode=disable&connect_timeout=2"
	err := orgRestoreConsole([]string{"-org", "acme", "-atl-db", bad}, quietLog())
	if err == nil {
		t.Fatal("restore-console succeeded against an unreachable database")
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Errorf("the error does not report a connection problem: %v", err)
	}
	if strings.Contains(err.Error(), "caller_identities") {
		t.Errorf("an unreachable database is reported as a problem reading the "+
			"console's identity, which sends the operator to the wrong place: %v", err)
	}
}
