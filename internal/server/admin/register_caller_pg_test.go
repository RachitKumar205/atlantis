package admin

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
)

// TestRegisterCallerReconcilesGrants exercises syncDefaultCapabilities against
// a real Postgres.
//
// It needs one because the interesting part is the SQL: `capability = ANY($2)
// AND capability <> ALL($3)` with two text[] parameters is the kind of
// expression that is either exactly right or silently matches nothing, and a
// revoke that silently matches nothing is a demotion that appears to work. No
// amount of Go-level testing distinguishes those two outcomes.
//
// Env-gated like the other live-PG tests in this repo:
//
//	ATLANTIS_TEST_PG=postgres://atlantis:pw@localhost:55432/atlantis?sslmode=disable \
//	  go test ./internal/server/admin/ -run RegisterCaller -v
func TestRegisterCallerReconcilesGrants(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the grant-reconciliation test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Close via t.Cleanup rather than defer, and register it first so LIFO
	// order runs it last. A deferred Close runs before every t.Cleanup, which
	// would leave the row deletion below executing against a closed pool —
	// silently, since the test has no reason to check that error, and the
	// leftover rows would then poison the next run against the same database.
	t.Cleanup(pool.Close)

	const caller = "test-grant-sync"
	cleanup := func() {
		if _, err := pool.Exec(ctx,
			`DELETE FROM atlantis.caller_identities WHERE caller = $1`, caller); err != nil {
			t.Errorf("cleanup %q: %v", caller, err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	svc := New(pool, Config{})
	held := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT capability FROM atlantis.caller_capabilities WHERE caller = $1`, caller)
		if err != nil {
			t.Fatalf("read grants: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate: %v", err)
		}
		sort.Strings(out)
		return out
	}
	register := func(canMutate bool) {
		t.Helper()
		_, err := svc.RegisterCaller(ctx, &adminpb.RegisterCallerRequest{
			Caller: caller, CanMutate: canMutate, CreatedBy: "grant-sync-test",
		})
		if err != nil {
			t.Fatalf("RegisterCaller(can_mutate=%v): %v", canMutate, err)
		}
	}

	// A caller registered with can_mutate must be able to apply. Before grants
	// existed this was implied by the boolean; now it has to be written, and a
	// registration that wrote only the identity row would produce a caller that
	// authenticates and is refused by every RPC.
	register(true)
	want := authz.Names(authz.DefaultCapabilities(true))
	sort.Strings(want)
	if got := held(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after can_mutate=true grants are %v, want %v", got, want)
	}

	// An out-of-band grant, the kind an operator makes by hand.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ($1, 'CAPABILITY_OPERATOR', 'test') ON CONFLICT DO NOTHING`, caller); err != nil {
		t.Fatalf("hand-grant operator: %v", err)
	}

	// Demotion has to actually revoke. If the revoke predicate matched nothing,
	// SCHEMA_APPLY would survive here and the demotion would be cosmetic.
	register(false)
	got := held()
	for _, revoked := range []string{"CAPABILITY_SCHEMA_APPLY", "CAPABILITY_SCHEMA_PLAN", "CAPABILITY_JOBS_WRITE"} {
		if contains(got, revoked) {
			t.Errorf("%s survived can_mutate=false; the revoke predicate matched nothing", revoked)
		}
	}
	for _, kept := range authz.Names(authz.DefaultCapabilities(false)) {
		if !contains(got, kept) {
			t.Errorf("%s was revoked by a demotion that should not touch it", kept)
		}
	}
	// And the hand-made grant must survive: re-registering a caller to fix a
	// typo in created_by should not silently strip authority someone granted on
	// purpose.
	if !contains(got, "CAPABILITY_OPERATOR") {
		t.Error("re-registration revoked an out-of-band CAPABILITY_OPERATOR grant")
	}

	// Promotion restores the mutating bundle without disturbing the rest.
	register(true)
	got = held()
	for _, c := range append(authz.Names(authz.DefaultCapabilities(true)), "CAPABILITY_OPERATOR") {
		if !contains(got, c) {
			t.Errorf("%s missing after re-promotion; grants are %v", c, got)
		}
	}

	// granted_at records when authority began. Re-registering is not a new
	// grant, so an upsert that refreshed the timestamp would erase the only
	// record of when this caller became able to apply.
	var distinct int
	if err := pool.QueryRow(ctx, `
SELECT count(DISTINCT granted_at) FROM atlantis.caller_capabilities
 WHERE caller = $1 AND capability = 'CAPABILITY_SCHEMA_READ'`, caller).Scan(&distinct); err != nil {
		t.Fatalf("count granted_at: %v", err)
	}
	if distinct != 1 {
		t.Errorf("SCHEMA_READ has %d distinct granted_at values across three registrations, want 1", distinct)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
