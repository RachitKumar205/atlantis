package pg

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The claim `partition by` makes is that PostgreSQL enforces tenant isolation,
// so a forgotten call site cannot leak and caller-authored SQL is covered by
// the same guarantee as a generated read. That claim is void if the role
// atlantis connects as can bypass RLS — and the failure mode is that
// everything looks correct: the policies exist, FORCE is set, the catalog
// agrees, and every read still returns every tenant's rows.
//
// These tests execute that, rather than asserting it. Both directions are
// covered: a role that bypasses genuinely sees through a forced policy, and a
// restricted role genuinely does not.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/storage/pg/ -run RolePriv -v

func rolePrivPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise role-privilege detection")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// FORCE ROW LEVEL SECURITY closes the ordinary owner exemption, which is why a
// reasonable person checks for FORCE, finds it, and stops looking. It does not
// close BYPASSRLS and it does not close superuser.
//
// This proves that on the same table, with the same forced policy, the answer
// depends entirely on who is asking — which is what makes the boot assertion
// load-bearing rather than defensive.
func TestRolePrivilegesDecideWhetherForcedRLSMeansAnything(t *testing.T) {
	pool, ctx := rolePrivPool(t)

	clean := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.rolepriv_docs`)
		_, _ = pool.Exec(ctx, `DROP ROLE IF EXISTS rolepriv_tenant`)
	}
	clean()
	t.Cleanup(clean)

	for _, sql := range []string{
		`CREATE TABLE atlantis.rolepriv_docs (id int primary key, tenant text not null, body text)`,
		`INSERT INTO atlantis.rolepriv_docs VALUES (1,'acme','acme secret'), (2,'globex','globex secret')`,
		`ALTER TABLE atlantis.rolepriv_docs ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.rolepriv_docs FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY rolepriv_isolation ON atlantis.rolepriv_docs
		   USING (tenant = atlantis.current_partition())
		   WITH CHECK (tenant = atlantis.current_partition())`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	// Confirm the table really is configured the way an operator would check.
	var enabled, forced bool
	if err := pool.QueryRow(ctx, `
SELECT relrowsecurity, relforcerowsecurity FROM pg_class
 WHERE oid = 'atlantis.rolepriv_docs'::regclass`).Scan(&enabled, &forced); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !enabled || !forced {
		t.Fatalf("fixture is wrong: enabled=%v forced=%v; the test would prove nothing",
			enabled, forced)
	}

	// --- as the role atlantis actually connects with -------------------------
	privs, err := DetectRolePrivileges(ctx, PgxRoleQuerier{Q: pool})
	if err != nil {
		t.Fatalf("DetectRolePrivileges: %v", err)
	}
	t.Logf("connecting role %q: superuser=%v bypassrls=%v",
		privs.Name, privs.Superuser, privs.BypassRLS)

	var seen int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.rolepriv_docs`).Scan(&seen); err != nil {
		t.Fatalf("read: %v", err)
	}

	if privs.CanEnforceRLS() {
		// No partition is set, so a role RLS applies to must see nothing.
		if seen != 0 {
			t.Errorf("the connecting role cannot bypass RLS, yet a forced policy with "+
				"no partition set returned %d rows. The policy is not being applied", seen)
		}
		if err := RequireIsolatedRole(privs); err != nil {
			t.Errorf("RequireIsolatedRole rejected a role that demonstrably enforces "+
				"RLS: %v", err)
		}
	} else {
		// This is the state the deployment is in today, and the point of the
		// assertion. Prove the consequence rather than asserting it.
		if seen != 2 {
			t.Errorf("role %q reports superuser=%v bypassrls=%v but saw %d of 2 rows; "+
				"the relationship between the role attributes and the leak is not what "+
				"this test assumes", privs.Name, privs.Superuser, privs.BypassRLS, seen)
		} else {
			t.Logf("confirmed: forced RLS is inert for this role — both tenants' rows " +
				"returned with no partition set")
		}
		if err := RequireIsolatedRole(privs); !errors.Is(err, ErrRoleBypassesRLS) {
			t.Errorf("RequireIsolatedRole accepted a role that sees through a forced "+
				"policy: %v", err)
		}
	}
}

// And the other direction: a properly restricted role is isolated by the same
// policy, so the assertion is not merely refusing everything.
func TestRestrictedRoleIsIsolatedByTheSamePolicy(t *testing.T) {
	pool, ctx := rolePrivPool(t)

	clean := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.rolepriv_iso`)
		_, _ = pool.Exec(ctx, `DROP OWNED BY rolepriv_tenant`)
		_, _ = pool.Exec(ctx, `DROP ROLE IF EXISTS rolepriv_tenant`)
	}
	clean()
	t.Cleanup(clean)

	for _, sql := range []string{
		`CREATE ROLE rolepriv_tenant LOGIN PASSWORD 'x' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`CREATE TABLE atlantis.rolepriv_iso (id int primary key, tenant text not null, body text)`,
		`INSERT INTO atlantis.rolepriv_iso VALUES (1,'acme','acme secret'), (2,'globex','globex secret')`,
		`ALTER TABLE atlantis.rolepriv_iso ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.rolepriv_iso FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY rolepriv_iso_pol ON atlantis.rolepriv_iso
		   USING (tenant = atlantis.current_partition())`,
		`GRANT USAGE ON SCHEMA atlantis TO rolepriv_tenant`,
		`GRANT SELECT ON atlantis.rolepriv_iso TO rolepriv_tenant`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE rolepriv_tenant`); err != nil {
		t.Fatalf("set role: %v", err)
	}

	// The restricted role must be one RequireIsolatedRole accepts.
	privs, err := DetectRolePrivileges(ctx, PgxRoleQuerier{Q: tx})
	if err != nil {
		t.Fatalf("DetectRolePrivileges: %v", err)
	}
	if privs.Name != "rolepriv_tenant" {
		t.Fatalf("SET LOCAL ROLE did not take effect; current_user is %q", privs.Name)
	}
	if err := RequireIsolatedRole(privs); err != nil {
		t.Fatalf("RequireIsolatedRole rejected a NOSUPERUSER NOBYPASSRLS role: %v", err)
	}

	// With no partition set, it must see nothing — fail closed.
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM atlantis.rolepriv_iso`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 0 {
		t.Errorf("with no partition set, a restricted role saw %d rows. An unset "+
			"discriminator must match nothing, or the absence of a tenant context "+
			"becomes access to every tenant", n)
	}

	// With a partition set, exactly its own row and no other tenant's.
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
		t.Fatalf("set_partition: %v", err)
	}
	rows, err := tx.Query(ctx, `SELECT tenant FROM atlantis.rolepriv_iso`)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, s)
	}
	rows.Close()

	if len(got) != 1 || got[0] != "acme" {
		t.Errorf("tenant acme saw %v; want exactly [acme]. Anything else is a "+
			"cross-tenant read through the policy that is supposed to prevent it", got)
	}
}

// BYPASSRLS on its own, with no superuser bit, must be caught too.
//
// The live deployment happens to fail on the superuser attribute, so a check
// that only looked at rolsuper would pass every test above while leaving the
// other half of the hole open. BYPASSRLS is the more insidious of the two: it
// is grantable, it does not look like a privilege escalation in a review, and
// nothing about the role otherwise says "sees every tenant's rows".
func TestBypassRLSAloneIsCaught(t *testing.T) {
	pool, ctx := rolePrivPool(t)

	clean := func() {
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.rolepriv_byp`)
		// DROP OWNED BY first: DROP ROLE refuses while the role holds any
		// grant, and the schema-level USAGE outlives the table. Skipping this
		// leaves the role behind and the next run fails on "already exists".
		_, _ = pool.Exec(ctx, `DROP OWNED BY rolepriv_bypasser`)
		_, _ = pool.Exec(ctx, `DROP ROLE IF EXISTS rolepriv_bypasser`)
	}
	clean()
	t.Cleanup(clean)

	for _, sql := range []string{
		`CREATE ROLE rolepriv_bypasser LOGIN PASSWORD 'x' NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE`,
		`CREATE TABLE atlantis.rolepriv_byp (id int primary key, tenant text not null)`,
		`INSERT INTO atlantis.rolepriv_byp VALUES (1,'acme'), (2,'globex')`,
		`ALTER TABLE atlantis.rolepriv_byp ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE atlantis.rolepriv_byp FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY rolepriv_byp_pol ON atlantis.rolepriv_byp
		   USING (tenant = atlantis.current_partition())`,
		`GRANT USAGE ON SCHEMA atlantis TO rolepriv_bypasser`,
		`GRANT SELECT ON atlantis.rolepriv_byp TO rolepriv_bypasser`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE rolepriv_bypasser`); err != nil {
		t.Fatalf("set role: %v", err)
	}

	privs, err := DetectRolePrivileges(ctx, PgxRoleQuerier{Q: tx})
	if err != nil {
		t.Fatalf("DetectRolePrivileges: %v", err)
	}
	if privs.Superuser {
		t.Fatalf("fixture is wrong: %q is a superuser, so this proves nothing about "+
			"BYPASSRLS on its own", privs.Name)
	}
	if !privs.BypassRLS {
		t.Fatalf("fixture is wrong: %q does not hold BYPASSRLS", privs.Name)
	}

	// It must actually see through the forced policy, or the attribute doesn't
	// mean what this test claims.
	var seen int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM atlantis.rolepriv_byp`).Scan(&seen); err != nil {
		t.Fatalf("read: %v", err)
	}
	if seen != 2 {
		t.Errorf("a BYPASSRLS role saw %d of 2 rows through a forced policy with no "+
			"partition set; the premise of this test is wrong", seen)
	}

	if privs.CanEnforceRLS() {
		t.Error("CanEnforceRLS reported true for a role that just read every tenant's rows")
	}
	if err := RequireIsolatedRole(privs); !errors.Is(err, ErrRoleBypassesRLS) {
		t.Errorf("RequireIsolatedRole accepted a NOSUPERUSER BYPASSRLS role: %v", err)
	}
}
