package pg

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Which tables would hide their rows from a statement that binds no tenant.
//
// # Why this needs a restricted role
//
// The question is about the TABLE and the ROLE together. Run as a superuser —
// which is what the `atlantis` role is on a development database — the answer
// is always "none", because row-level security does not apply. A test that
// connected as the default role would pass with the whole function deleted.
//
// # What this is for
//
// The backfill worker issues a chunked UPDATE with no tenant bound. Where the
// answer here is "blocked", that UPDATE matches zero rows, reports success, and
// the follow-on SET NOT NULL fails on rows nothing ever touched.
//
// A first version of that guard asked the SCHEMA instead — `partition by`
// declared or not — and a review executed three cases where the two answers
// disagree. Cases 2 and 3 below are two of them; case 4 is the mirror the
// schema test missed entirely.
func TestRLSBlockedTables(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the row-level security probe")
	}
	ctx := context.Background()

	admin, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		for _, tbl := range []string{"rlsb_forced", "rlsb_nopolicy", "rlsb_noforce", "rlsb_orphan"} {
			_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.`+tbl+` CASCADE`)
		}
		pgcatalog.Exec(t, dsn,
			`DROP OWNED BY rlsb_probe`,
			`DROP ROLE IF EXISTS rlsb_probe`)
	}
	drop()
	t.Cleanup(drop)

	if _, err := admin.Exec(ctx, `
-- 1. Enforced. An unbound statement sees nothing.
CREATE TABLE atlantis.rlsb_forced (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.rlsb_forced ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.rlsb_forced FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.rlsb_forced USING (tenant = atlantis.current_partition());

-- 2. THE MODAL CASE. partition by was added to an entity that already
-- existed, so no differ emitted the policy. The backfill works fine here, and
-- a schema-based guard refused it.
CREATE TABLE atlantis.rlsb_nopolicy (id bigint primary key, tenant text not null);

-- 3. A policy, enabled, but not FORCED. The owner is exempt, and atlantis owns
-- the tables it creates, so the backfill sees every row.
CREATE TABLE atlantis.rlsb_noforce (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.rlsb_noforce ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.rlsb_noforce USING (tenant = atlantis.current_partition());

-- 4. THE MIRROR CASE. The entity dropped partition by; removal emits nothing,
-- so the table keeps its policy. The schema says unpartitioned and the table
-- still hides every row — missed entirely by a schema-based guard.
CREATE TABLE atlantis.rlsb_orphan (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.rlsb_orphan ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.rlsb_orphan FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.rlsb_orphan USING (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE rlsb_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO rlsb_probe;
GRANT SELECT, UPDATE ON atlantis.rlsb_forced, atlantis.rlsb_nopolicy,
                        atlantis.rlsb_noforce, atlantis.rlsb_orphan TO rlsb_probe;`)
		return err
	})
	// The probing role must OWN the tables, which is the production shape:
	// atlantis creates the tables it serves and connects as their owner. It is
	// also what makes FORCE load-bearing — without ownership, case 3 would be
	// indistinguishable from case 1.
	for _, tbl := range []string{"rlsb_forced", "rlsb_nopolicy", "rlsb_noforce", "rlsb_orphan"} {
		if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.`+tbl+` OWNER TO rlsb_probe`); err != nil {
			t.Fatalf("hand %s to the probing role: %v", tbl, err)
		}
	}

	restricted, err := New(ctx, DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//rlsb_probe:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the restricted role: %v", err)
	}
	t.Cleanup(restricted.Close)

	names := []string{
		"atlantis.rlsb_forced", "atlantis.rlsb_nopolicy",
		"atlantis.rlsb_noforce", "atlantis.rlsb_orphan",
	}
	blocked, err := RLSBlockedTables(ctx, restricted, names)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	got := map[string]bool{}
	for _, b := range blocked {
		got[b] = true
	}

	for _, tc := range []struct {
		table string
		want  bool
		why   string
	}{
		{"atlantis.rlsb_forced", true,
			"enforced, so an unbound chunk matches nothing and reports success"},
		{"atlantis.rlsb_nopolicy", false,
			"no policy exists — the modal case for an entity that gained " +
				"`partition by` after it was created. The backfill works"},
		{"atlantis.rlsb_noforce", false,
			"the owner is exempt without FORCE, and the worker connects as the owner"},
		{"atlantis.rlsb_orphan", true,
			"the entity dropped `partition by` and the table kept its policy; a " +
				"schema-based guard reports this table as safe and it is not"},
	} {
		if got[tc.table] != tc.want {
			t.Errorf("%s: blocked = %v, want %v — %s", tc.table, got[tc.table], tc.want, tc.why)
		}
	}

	// As a role that bypasses row-level security, nothing is blocked. This is a
	// supported deployment: RequireIsolatedRole rejects that posture only under
	// ATL_REQUIRE_TENANT_ISOLATION.
	asAdmin, err := RLSBlockedTables(ctx, admin, names)
	if err != nil {
		t.Fatalf("probe as the default role: %v", err)
	}
	if len(asAdmin) != 0 {
		t.Errorf("a role that bypasses row-level security reported %v as blocked. "+
			"The policy does not apply to it, so its backfill sees every row",
			asAdmin)
	}
}
