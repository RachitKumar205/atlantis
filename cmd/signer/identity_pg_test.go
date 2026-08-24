package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/migrations"
)

// The signer's registration check, against a real schema.
//
// # Why this did not exist before, and why that was a hole
//
// The check was one SELECT inline in handleIssue, reachable only through a
// signed request with CA material behind it. The fuzz test sets pgPool to nil
// and notes the identity check is "tested elsewhere"; elsewhere was nowhere. So
// the query the signer relies on to refuse unregistered callers was never
// executed by any test, and a typo in it would have surfaced as every issuance
// failing in a deployment.
//
// It matters more now. The signer reads a view rather than the table, because a
// revoked caller keeps its row — and a signer still issuing certificates to a
// revoked caller is a revocation that is contradicted by the component that
// hands out credentials.

func signerIdentityDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the signer's identity check")
	}
	const dbName = "atlantis_signer_identity"
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, dbName)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.RunFS(dsn, migrations.Infra, "infra",
		migrate.InfraHistoryTable, quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// callerMayBeIssuedTo reads the package-level pool, the same one run()
	// builds. Restored afterwards so a test that runs after this one does not
	// inherit a closed pool.
	saved := pgPool
	pgPool = pool
	t.Cleanup(func() { pgPool = saved })
	return pool
}

// A registered caller is issued to; an unknown one is not.
//
// The baseline, and the thing that proves the query runs at all. Without the
// negative case a function that returned true unconditionally would pass.
func TestTheSignerIssuesOnlyToRegisteredCallers(t *testing.T) {
	pool := signerIdentityDB(t)
	ctx := context.Background()

	const caller = "signer-identity-live"
	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
		 VALUES ($1, false, 'signer-test')`, caller); err != nil {
		t.Fatalf("seed caller: %v", err)
	}

	ok, err := callerMayBeIssuedTo(ctx, caller)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok {
		t.Error("a registered caller was refused a certificate")
	}

	ok, err = callerMayBeIssuedTo(ctx, "signer-identity-never-registered")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Error("an unregistered caller would be issued a certificate")
	}
}

// The one this change exists for: a revoked caller gets nothing.
//
// Reading the table instead of the view leaves this test failing, which is the
// point — the row is still there, so the table's answer is "registered".
func TestTheSignerRefusesARevokedCaller(t *testing.T) {
	pool := signerIdentityDB(t)
	ctx := context.Background()

	const caller = "signer-identity-revoked"
	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
		 VALUES ($1, false, 'signer-test')`, caller); err != nil {
		t.Fatalf("seed caller: %v", err)
	}
	if ok, err := callerMayBeIssuedTo(ctx, caller); err != nil || !ok {
		t.Fatalf("the caller is not issuable before being revoked (ok=%v err=%v); "+
			"this test would pass for the wrong reason", ok, err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE atlantis.caller_identities SET revoked_at = NOW() WHERE caller = $1`,
		caller); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	ok, err := callerMayBeIssuedTo(ctx, caller)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Error("the signer would issue a fresh certificate to a revoked caller, so " +
			"a revocation the server enforces is undone by the signer")
	}

	// And the row really is still present — otherwise this is testing deletion,
	// not revocation, and the view is doing nothing.
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM atlantis.caller_identities WHERE caller = $1)`,
		caller).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("the revoked caller's row is gone, so the refusal above proves nothing " +
			"about the view")
	}
}
