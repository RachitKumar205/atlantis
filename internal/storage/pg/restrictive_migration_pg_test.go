package pg

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Migration 0025 moves the tenant boundary from PERMISSIVE to RESTRICTIVE
// without ever widening the table, and without rewriting the operator's
// predicate.
//
// # Why this runs the real .sql
//
// The migration is a DO block full of format() and catalogue lookups. A Go
// re-implementation of it would be a second thing to get wrong, and the thing
// shipped to customers is the file. So the file is what executes here.
//
// # What is actually at risk
//
// Two failures, neither of which the end state reveals:
//
//   - A WIDER INTERMEDIATE STATE. Drop the permissive boundary before the
//     restrictive one exists and the table is unisolated for the width of one
//     statement. Inside a transaction that is invisible; down_sql and this
//     migration are also things operators run by hand.
//   - A REGENERATED PREDICATE. Rebuilding `col = current_partition()` from the
//     declared column silently discards an operator's own hardening — the
//     `deleted_at IS NULL` below — and makes soft-deleted rows visible to every
//     caller, while the migration reports success.
//
// The fixture carries that hardening precisely so the second failure cannot
// pass unnoticed.
func TestMigration0025MovesTheBoundaryWithoutWideningIt(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the restrictive-policy migration")
	}
	ctx := context.Background()
	admin, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.m25t_doc CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY m25t_probe`, `DROP ROLE IF EXISTS m25t_probe`)
	}
	drop()
	t.Cleanup(drop)

	// The OLD shape: one permissive policy carrying the boundary, hardened by
	// the operator with a clause atlantis never emits.
	if _, err := admin.Exec(ctx, `
CREATE TABLE atlantis.m25t_doc (id int PRIMARY KEY, tenant text NOT NULL, deleted_at timestamptz);
INSERT INTO atlantis.m25t_doc VALUES (1,'acme',NULL),(2,'globex',NULL),(3,'acme',now());
ALTER TABLE atlantis.m25t_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.m25t_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY m25t_doc_tenant_isolation ON atlantis.m25t_doc
  USING (deleted_at IS NULL AND tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE m25t_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO m25t_probe;
GRANT SELECT, INSERT ON atlantis.m25t_doc TO m25t_probe;`)
		return err
	})
	// Ownership matters: FORCE is what makes the policy apply to the owner, and
	// atlantis owns the tables it serves.
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.m25t_doc OWNER TO m25t_probe`); err != nil {
		t.Fatalf("hand the table over: %v", err)
	}

	sql, err := os.ReadFile("../../../migrations/infra/0025_restrictive_tenant_policy.up.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := admin.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("run the migration: %v", err)
	}

	// The boundary is restrictive now, and still says what it said before.
	var permissive bool
	var qual string
	if err := admin.QueryRow(ctx, `
SELECT p.polpermissive, pg_get_expr(p.polqual, p.polrelid)
  FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = 'm25t_doc' AND p.polname = 'm25t_doc_tenant_isolation'`).Scan(&permissive, &qual); err != nil {
		t.Fatalf("read the boundary: %v", err)
	}
	if permissive {
		t.Error("the boundary is still PERMISSIVE. Any other permissive policy " +
			"ORs with it, so a user's access-control grant reopens every tenant")
	}
	if !strings.Contains(qual, "deleted_at") {
		t.Errorf("the migration rewrote the predicate instead of carrying it: %q.\n"+
			"The operator's `deleted_at IS NULL` clause is gone, so soft-deleted "+
			"rows are now visible to every caller and the migration said nothing", qual)
	}

	// And the replaceable grant exists, or the table denies everything.
	var grants int
	if err := admin.QueryRow(ctx, `
SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = 'm25t_doc' AND p.polpermissive`).Scan(&grants); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if grants == 0 {
		t.Fatal("no permissive policy survives, so this table now denies every " +
			"row to every caller. Restrictive policies only ever narrow")
	}

	// THE POINT OF THE WHOLE CHANGE: a hostile grant, of the kind a user writes
	// for RBAC and the old model had to refuse outright, cannot cross the
	// boundary.
	if _, err := admin.Exec(ctx, `
CREATE POLICY m25t_user_rbac ON atlantis.m25t_doc AS PERMISSIVE USING (true) WITH CHECK (true)`); err != nil {
		t.Fatalf("add the user grant: %v", err)
	}

	restricted, err := New(ctx, DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//m25t_probe:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the restricted role: %v", err)
	}
	t.Cleanup(restricted.Close)

	tx, err := restricted.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var ids string
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(string_agg(id::text, ',' ORDER BY id), '') FROM atlantis.m25t_doc`).Scan(&ids); err != nil {
		t.Fatalf("bound read: %v", err)
	}
	// 2 is another tenant's. 3 is acme's but soft-deleted, and only the carried
	// clause excludes it — proving the predicate survived in behaviour, not just
	// in catalogue text.
	if ids != "1" {
		t.Errorf("bound read returned %q, want \"1\". A permissive `USING (true)` "+
			"grant crossed the tenant boundary, or the carried `deleted_at` "+
			"clause was lost", ids)
	}

	var unbound int
	if err := restricted.QueryRow(ctx, `SELECT count(*) FROM atlantis.m25t_doc`).Scan(&unbound); err != nil {
		t.Fatalf("unbound read: %v", err)
	}
	if unbound != 0 {
		t.Errorf("an unbound read returned %d rows, want 0", unbound)
	}
}
