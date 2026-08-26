// Package pgcatalog serialises tests that write PostgreSQL's shared catalogs.
//
// Creating a role, dropping a role, or granting on a schema updates rows in
// pg_authid and pg_namespace. Two test binaries doing that at the same instant
// fail with `tuple concurrently updated` (SQLSTATE XX000) — an error that names
// nothing about the test and reads as a defect in whatever was under test.
//
// `go test` runs packages in parallel, and several of them create roles, so the
// collision is reachable wherever ATLANTIS_TEST_PG is set. The rate is
// load-dependent: 0 failures in 12 runs at -p 8, and 1 in 12 with the package
// binaries launched simultaneously.
//
// A package rather than a helper per file: mutual exclusion holds only if every
// writer takes the same lock, and an unexported const cannot be named from
// another package.
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
// On its own connection: a session-level advisory lock belongs to the backend
// that took it, so through a pool the unlock can land on a different backend
// and leak the lock. Closing the connection ends the session and drops the lock
// even when the unlock statement fails.
//
// The lock covers the whole callback. DROP ROLE is a catalog write too, and
// t.Cleanup runs after the test function's defers.
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
