package admin

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Revocation, after migration 0033 stopped it deleting the caller.
//
// Every property here is a property of SQL, which is why they need a real
// Postgres. Capabilities surviving a revoke is the absence of a cascade, and a
// stub that does not delete passes a Go-level test whether or not the real
// schema still cascades.
//
// The other half is the two views. `(regs UNION ids) EXCEPT revoked` either
// subtracts the right set or quietly subtracts nothing, and a revocation that
// subtracts nothing looks exactly like a revocation that worked until somebody
// checks whether the caller can still authenticate.

func revokeTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the revocation tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered before any row cleanup so LIFO runs it last — a deferred Close
	// would run first and leave the deletions executing against a closed pool,
	// silently, poisoning the next run against this database.
	t.Cleanup(pool.Close)
	return pool, ctx
}

// seedRevokeCaller puts one caller in place with a grant that registration does
// not manage.
//
// CAPABILITY_OPERATOR specifically, because it is outside ManagedCapabilities:
// re-registering does not restore it, so a revoke that destroys it leaves no
// ordinary path back.
func seedRevokeCaller(t *testing.T, pool *pgxpool.Pool, ctx context.Context, caller string) {
	t.Helper()
	drop := func() {
		if _, err := pool.Exec(ctx,
			`DELETE FROM atlantis.caller_identities WHERE caller = $1`, caller); err != nil {
			t.Errorf("cleanup identity %q: %v", caller, err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM atlantis.caller_registrations WHERE caller = $1`, caller); err != nil {
			t.Errorf("cleanup registrations %q: %v", caller, err)
		}
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
		 VALUES ($1, false, 'revoke-test')`, caller); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
		 VALUES ($1, 'CAPABILITY_OPERATOR', 'revoke-test')`, caller); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
}

func capCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, caller string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.caller_capabilities WHERE caller = $1`, caller).Scan(&n); err != nil {
		t.Fatalf("count capabilities: %v", err)
	}
	return n
}

func inActiveCallers(t *testing.T, pool *pgxpool.Pool, ctx context.Context, caller string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM atlantis.active_callers WHERE caller = $1)`, caller).Scan(&ok); err != nil {
		t.Fatalf("query active_callers: %v", err)
	}
	return ok
}

// The fix itself: revoking keeps the grants.
//
// Deleting the row cascaded caller_capabilities away (0018's ON DELETE CASCADE),
// which took CAPABILITY_OPERATOR with it. Nothing restored that — registration
// leaves unmanaged grants alone by design — so a revoked-then-restored caller
// came back authenticating and then failing authz on one page.
func TestRevokingACallerKeepsItsCapabilities(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-caps"
	seedRevokeCaller(t, pool, ctx, caller)

	if got := capCount(t, pool, ctx, caller); got != 1 {
		t.Fatalf("seed left %d capabilities, want 1", got)
	}

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if got := capCount(t, pool, ctx, caller); got != 1 {
		t.Errorf("revoking left %d capabilities, want 1 — the cascade is back, and "+
			"CAPABILITY_OPERATOR is a grant nothing else restores", got)
	}
}

// Revocation still cuts the caller off. The point of keeping the row is that
// restoring is possible, not that revoking is softer.
func TestRevokingACallerStopsItAuthenticating(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-cutoff"
	seedRevokeCaller(t, pool, ctx, caller)

	if !inActiveCallers(t, pool, ctx, caller) {
		t.Fatal("the seeded caller is not in active_callers, so this test would pass " +
			"for the wrong reason")
	}

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if inActiveCallers(t, pool, ctx, caller) {
		t.Error("a revoked caller is still in active_callers, so the auth allowlist " +
			"would keep admitting it")
	}
	// The interceptor's own path, which is a different view — narrower, because
	// it requires an identity row rather than any registration. Both have to
	// refuse, and checking only one would miss a caller cut off from the
	// allowlist while the binding check still waved it through.
	b, err := svc.LookupCallerCertBinding(ctx, caller)
	if err != nil {
		t.Fatalf("lookup binding: %v", err)
	}
	if b.Exists {
		t.Error("LookupCallerCertBinding still reports a revoked caller as existing, " +
			"so the cert-binding interceptor would admit it")
	}
}

// A revoked caller must not be readmitted by its own next deploy.
//
// caller_registrations is written by `tide apply`, not by an operator, and the
// allowlist is a union over it. Without the EXCEPT in active_callers, revoking a
// caller that still ships schema would last exactly until its next pipeline run
// — revocation undone by the thing being revoked, silently, on a delay.
func TestARevokedCallerIsNotReadmittedByApplyingSchema(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-readmit"
	seedRevokeCaller(t, pool, ctx, caller)

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Exactly what `tide apply` writes.
	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
		 VALUES ($1, 'schema.atl', '', '') ON CONFLICT DO NOTHING`, caller); err != nil {
		t.Fatalf("simulate tide apply: %v", err)
	}

	if inActiveCallers(t, pool, ctx, caller) {
		t.Error("applying schema put a revoked caller back in active_callers — a " +
			"revocation that expires on the next deploy is not a revocation")
	}
}

// Revoking a caller that only ever applied schema still cuts it off.
//
// It has no identity row to mark, so the revoke has to write a tombstone. Left
// undone, deleting the registrations would cut the caller off only until it
// applied again — the same readmission as above, reached from the other side.
func TestRevokingARegistrationOnlyCallerLeavesATombstone(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-tombstone"
	drop := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.caller_identities WHERE caller = $1`, caller)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.caller_registrations WHERE caller = $1`, caller)
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
		 VALUES ($1, 'schema.atl', '', '')`, caller); err != nil {
		t.Fatalf("seed registration: %v", err)
	}
	if !inActiveCallers(t, pool, ctx, caller) {
		t.Fatal("a registration-only caller is not in active_callers to begin with")
	}

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Re-apply, which is what would readmit it without a tombstone.
	if _, err := pool.Exec(ctx,
		`INSERT INTO atlantis.caller_registrations (caller, file_path, content, sha256)
		 VALUES ($1, 'schema.atl', '', '') ON CONFLICT DO NOTHING`, caller); err != nil {
		t.Fatalf("simulate tide apply: %v", err)
	}
	if inActiveCallers(t, pool, ctx, caller) {
		t.Error("a registration-only caller came back after being revoked; the revoke " +
			"left no tombstone for the EXCEPT to match")
	}
}

// Restoring is clearing one column, and everything is where it was.
//
// This is the claim migration 0033 is built on: nothing has to be rebuilt
// because nothing was destroyed. It matters most for 'atlantis-console', whose
// grants are spread across migrations and whose CN RegisterCaller refuses — so
// reconstruction is not available to it at all.
func TestRestoringARevokedCallerReturnsItIntact(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-restore"
	seedRevokeCaller(t, pool, ctx, caller)

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE atlantis.caller_identities SET revoked_at = NULL WHERE caller = $1`, caller); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !inActiveCallers(t, pool, ctx, caller) {
		t.Error("a restored caller is still not in active_callers")
	}
	if got := capCount(t, pool, ctx, caller); got != 1 {
		t.Errorf("a restored caller holds %d capabilities, want 1 — CAPABILITY_OPERATOR "+
			"did not survive the round trip", got)
	}
}

// Re-registering a revoked caller is refused rather than silently ineffective.
//
// The upsert does not clear revoked_at, so without the guard RegisterCaller
// returns success and the caller stays cut off. Refusing is the safe direction:
// registration is routine and revocation is not, so the routine act must not
// undo the other one.
func TestRegisteringARevokedCallerIsRefused(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-reregister"
	seedRevokeCaller(t, pool, ctx, caller)

	svc := New(pool, Config{})
	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err := svc.RegisterCaller(ctx, &adminpb.RegisterCallerRequest{
		Caller: caller, CanMutate: false, CreatedBy: "revoke-test",
	})
	if err == nil {
		t.Fatal("re-registering a revoked caller succeeded; it would report success " +
			"while leaving the caller revoked")
	}
	// The message has to say what to do instead. "caller is revoked" alone tells
	// an operator that their command failed, not how to get the caller back.
	if !strings.Contains(err.Error(), "restore") {
		t.Errorf("the refusal does not point at restoring: %v", err)
	}
	if inActiveCallers(t, pool, ctx, caller) {
		t.Error("the refused registration un-revoked the caller anyway")
	}
}

// GetCallers has to say that a revoked caller is revoked.
//
// Before 0033 a revoked caller vanished from this list, so "gone" was the
// signal. Now the row survives, and without RevokedAt on the wire a cut-off
// caller renders identically to a working one — an operator reading the console
// would see nothing wrong while every one of that caller's requests failed.
//
// It is also the shape of bug that compiles: a column added to the SELECT and
// not to the Scan, or the reverse, is a runtime error in pgx rather than a
// build one, and the surrounding tests would not notice either.
func TestGetCallersReportsRevokedState(t *testing.T) {
	pool, ctx := revokeTestPool(t)
	const caller = "test-revoke-listed"
	seedRevokeCaller(t, pool, ctx, caller)

	svc := New(pool, Config{})
	find := func() *adminpb.CallerInfo {
		t.Helper()
		resp, err := svc.GetCallers(ctx, &adminpb.GetCallersRequest{})
		if err != nil {
			t.Fatalf("GetCallers: %v", err)
		}
		for _, c := range resp.GetCallers() {
			if c.GetCaller() == caller {
				return c
			}
		}
		return nil
	}

	before := find()
	if before == nil {
		t.Fatal("the seeded caller is not listed at all, so this test proves nothing")
	}
	if before.GetRevokedAt() != "" {
		t.Errorf("a live caller reports revoked_at %q, want empty", before.GetRevokedAt())
	}

	if _, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: caller}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	after := find()
	if after == nil {
		t.Fatal("a revoked caller disappeared from GetCallers; an operator cannot " +
			"restore what the list will not show them")
	}
	if after.GetRevokedAt() == "" {
		t.Error("a revoked caller reports an empty revoked_at, so it is indistinguishable " +
			"from a working one in the console")
	}
}
