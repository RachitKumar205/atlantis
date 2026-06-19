package auth

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestReload_UnionsRegistrationsAndIdentities pins the union semantics:
// the allowlist must accept a caller present only in caller_identities —
// the exact case of a dev/runtime backend pre-registered by an operator
// that never applies schema (so it never lands in caller_registrations).
// Before the union, such a caller hit "not in allowlist".
//
// Env-gated like the introspect live-PG tests; point it at any atlantis DB:
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/auth/ -run Reload -v
func TestReload_UnionsRegistrationsAndIdentities(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the allowlist union test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	// Column subsets match the real migrations; IF NOT EXISTS so the test
	// runs against a live atlantis DB without clobbering its tables.
	mustExec(`CREATE SCHEMA IF NOT EXISTS atlantis`)
	mustExec(`CREATE TABLE IF NOT EXISTS atlantis.caller_registrations (
		caller TEXT NOT NULL, file_path TEXT NOT NULL, content TEXT NOT NULL,
		sha256 TEXT NOT NULL, submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (caller, file_path))`)
	mustExec(`CREATE TABLE IF NOT EXISTS atlantis.caller_identities (
		caller TEXT PRIMARY KEY, can_mutate BOOLEAN NOT NULL DEFAULT false,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), created_by TEXT NOT NULL DEFAULT '',
		cert_fingerprint BYTEA)`)

	const applied = "test-applied-caller"   // in caller_registrations only
	const readOnly = "test-readonly-caller" // in caller_identities only
	mustExec(`INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
		VALUES ($1, 'x.atl', '', '') ON CONFLICT DO NOTHING`, applied)
	mustExec(`INSERT INTO atlantis.caller_identities (caller, can_mutate)
		VALUES ($1, false) ON CONFLICT DO NOTHING`, readOnly)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.caller_registrations WHERE caller = $1`, applied)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.caller_identities WHERE caller = $1`, readOnly)
	})

	a := New(pool, nil)
	if err := a.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !a.Allows(applied) {
		t.Errorf("caller in caller_registrations must be allowed")
	}
	if !a.Allows(readOnly) {
		t.Errorf("caller in caller_identities only must be allowed (read-only runtime CN)")
	}
	if a.Allows("test-unknown-caller") {
		t.Errorf("caller in neither table must be rejected")
	}
}
