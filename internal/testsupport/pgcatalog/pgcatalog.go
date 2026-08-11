// Package pgcatalog serialises tests that write PostgreSQL's shared catalogs.
//
// Creating a role, dropping a role, or granting on a schema updates rows in
// pg_authid and pg_namespace. Two test binaries doing that at the same instant
// fail with `tuple concurrently updated` (SQLSTATE XX000) — an error that names
// nothing about the test and reads as a defect in whatever was under test.
//
// It was a latent nuisance while CI ran no database. It stops being latent the
// moment ATLANTIS_TEST_PG is set in the workflow, because `go test` runs
// packages in parallel and four of them create roles.
//
// The rate is load-dependent and was overstated here. "3 runs in 12" was
// written from one observation; a later review could not reproduce it — 0
// failures in 12 runs at -p 8, and 1 in 12 only with the package binaries
// launched simultaneously, where the error was a fixture collision rather than
// the `tuple concurrently updated` this was written about. The hazard is real
// and the lock is cheap; the number was not measured well enough to state.
//
// # Why this is a package rather than a helper per file
//
// Mutual exclusion only works if every writer takes the same lock. The first
// attempt at this was a const in one package's test file with a comment saying
// other tests "should take the same key" — which no other package could even
// name, because an unexported const is not reachable from another package. A
// rule that has to be re-implemented correctly in four places is a rule that
// will be implemented in three.
//
// (An earlier version of this comment said "two of eight sites took it; the
// other six collided as before". That described a state no reader can find in
// the history — the const version was never committed. Removed rather than
// left as an unverifiable claim.)
package pgcatalog

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// lockKey is the advisory-lock key. Arbitrary and fixed forever: two callers
// that disagree about it do not exclude each other.
const lockKey int64 = 8021977453110002

// Do runs fn while holding the catalog lock, on a connection of its own.
//
// # Why a dedicated connection
//
// A session-level advisory lock belongs to the backend that took it. Taking one
// through a pool means the unlock can land on a different backend, and the lock
// leaks: the next caller then blocks forever, and with no statement timeout the
// run hangs until the test binary's own panic. A review reproduced exactly that
// — lock on backend 18987, unlock on 18990 returning false, one lock still
// held.
//
// A dedicated connection makes release unconditional. Even if the unlock
// statement fails, closing the connection ends the session and PostgreSQL drops
// the lock. That is the property worth having: the failure mode of a leaked
// exclusion lock is a hang, which is far worse than the collision it prevents.
//
// # Why the whole callback, not just the CREATE
//
// The DROP is a catalog write too. The first version of this took the lock
// around CREATE ROLE only and dropped the role from t.Cleanup — which Go runs
// AFTER the test function's defers, so the unlock had already happened. Two of
// three catalog writes ran unprotected, in a test whose comment named DROP ROLE
// as the hazard.
func Do(t *testing.T, dsn string, fn func(conn *pgx.Conn) error) {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgcatalog: connect: %v", err)
	}
	// Unconditional release: closing the session drops the lock whatever else
	// happened.
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatalf("pgcatalog: take the lock: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	if err := fn(conn); err != nil {
		t.Fatalf("pgcatalog: %v", err)
	}
}

// Exec is Do for the common case: a batch of catalog statements.
//
// Errors are reported, not fatal, because these run in cleanup paths where the
// object may already be gone and a failure there should not mask the real
// result of the test.
func Exec(t *testing.T, dsn string, statements ...string) {
	t.Helper()
	Do(t, dsn, func(conn *pgx.Conn) error {
		for _, sql := range statements {
			if _, err := conn.Exec(context.Background(), sql); err != nil {
				t.Logf("pgcatalog: %s: %v", sql, err)
			}
		}
		return nil
	})
}
