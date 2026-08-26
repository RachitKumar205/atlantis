package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// A foreign permissive policy must COMPOSE with tenant isolation, not defeat it
// and not block the migration.
//
// With the boundary as a PERMISSIVE policy, PostgreSQL ORs permissive policies,
// so a table carrying `USING (true)` applies the migration cleanly and isolates
// nothing. The only defence available then is to abort the migration whenever
// another permissive policy exists.
//
// That defence makes user-defined access control impossible. A permissive
// policy is what an RBAC grant is, so the first one anyone writes stops `tide
// apply` working on that table — and the advice to make them RESTRICTIVE
// inverts the logic, because restrictive policies AND and cannot express
// "admins OR auditors may read this".
//
// The boundary is restrictive now. A permissive policy is a grant, bounded by
// the tenant, and there is nothing left to refuse. So this file asserts the
// property that replaced the guard rather than the guard, and the assertion is
// behavioural: the hostile policy is present, and the rows still do not cross.
func TestForeignPermissivePolicyCannotCrossTheTenantBoundary(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise policy composition")
	}
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pguard_doc CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY pguard_probe`, `DROP ROLE IF EXISTS pguard_probe`)
	}
	drop()
	t.Cleanup(drop)

	before := lower(t, `
entity Doc in pguard {
  id     bigint primary
  tenant varchar(16) not null
  body   text
}
`)
	after := lower(t, `
entity Doc in pguard {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`)
	AssignProtoNumbers(nil, before)
	AssignProtoNumbers(before, after)

	initial, err := EmitInitial(before)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v", err)
	}
	if _, err := admin.Exec(ctx, `
INSERT INTO atlantis.pguard_doc VALUES (1,'acme','acme-secret'), (2,'globex','globex-secret')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The policy that used to be fatal, seeded BEFORE the migration: the shape a
	// database being adopted for tenant isolation plausibly already carries, and
	// the shape an RBAC grant takes.
	if _, err := admin.Exec(ctx, `
ALTER TABLE atlantis.pguard_doc ENABLE ROW LEVEL SECURITY;
CREATE POLICY legacy_app_access ON atlantis.pguard_doc FOR ALL USING (true) WITH CHECK (true);`); err != nil {
		t.Fatalf("seed the legacy policy: %v", err)
	}

	scripts, err := EmitSQL(before, after, ComputeDiff(before, after))
	if err != nil {
		t.Fatalf("EmitSQL: %v", err)
	}
	if _, err := admin.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("the migration refused a table carrying a permissive policy: %v\n\n"+
			"That refusal is what emitForeignPolicyGuard used to do, and removing it "+
			"is the point of the restrictive boundary — a user's access-control "+
			"grant must not stop `tide apply` working on their table.\n%s", err, scripts.Up)
	}

	// A restricted role that owns the table, so FORCE applies to it. As a
	// superuser every policy here is inert and this passes with the boundary
	// deleted.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE pguard_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO pguard_probe;
GRANT SELECT, INSERT ON atlantis.pguard_doc TO pguard_probe;`)
		return err
	})
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.pguard_doc OWNER TO pguard_probe`); err != nil {
		t.Fatalf("hand the table over: %v", err)
	}
	probe, err := pg.New(ctx, pg.DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//pguard_probe:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the probe role: %v", err)
	}
	t.Cleanup(probe.Close)

	t.Run("a bound caller reads only its own rows, with USING (true) present", func(t *testing.T) {
		tx, err := probe.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
			t.Fatalf("bind: %v", err)
		}
		var bodies string
		if err := tx.QueryRow(ctx,
			`SELECT coalesce(string_agg(body, ',' ORDER BY id), '') FROM atlantis.pguard_doc`).Scan(&bodies); err != nil {
			t.Fatalf("read: %v", err)
		}
		if bodies != "acme-secret" {
			t.Errorf("read %q, want \"acme-secret\". legacy_app_access is "+
				"`USING (true)`, so if the boundary is permissive the two OR "+
				"together and every tenant's rows come back", bodies)
		}
	})

	t.Run("an unbound caller reads nothing, with USING (true) present", func(t *testing.T) {
		var n int
		if err := probe.QueryRow(ctx, `SELECT count(*) FROM atlantis.pguard_doc`).Scan(&n); err != nil {
			t.Fatalf("read: %v", err)
		}
		if n != 0 {
			t.Errorf("an unbound read returned %d rows through the permissive policy", n)
		}
	})

	t.Run("a forged cross-tenant insert is refused, with WITH CHECK (true) present", func(t *testing.T) {
		tx, err := probe.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
			t.Fatalf("bind: %v", err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO atlantis.pguard_doc VALUES (9,'globex','forged')`)
		if err == nil {
			t.Fatal("a caller bound to acme wrote a row stamped globex. " +
				"legacy_app_access carries WITH CHECK (true); the boundary's own " +
				"WITH CHECK is the only thing that can refuse this")
		}
		if !strings.Contains(err.Error(), "tenant_isolation") {
			t.Errorf("the write was refused by something other than the tenant "+
				"boundary, so this proves nothing about it: %v", err)
		}
	})
}
