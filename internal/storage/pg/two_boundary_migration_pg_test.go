package pg

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Migration 0030 against a table carrying more than one permissive policy that
// calls current_partition().
//
// 0030's loop mutates pg_policy while iterating a cursor over pg_policy. Each
// iteration renames the boundary to `<name>_legacy` — still permissive, still
// calling current_partition(), so still satisfying the loop's own WHERE — then
// creates, grants, and drops. Whether that terminates rests on PL/pgSQL cursor
// snapshot behaviour the migration does not state. A single-policy fixture
// cannot tell the two apart: with one row there is nothing for a re-scan to
// find that a snapshot would not also find.
//
// Two policies separate them. A cursor that re-scans reaches, on the second
// pass, a `_legacy` policy the first pass already dropped, and the migration
// fails on ALTER POLICY; the outcome that fails quietly is a boundary left
// permissive.
//
// The pair is expressible: the emitted boundary plus a second hand-written
// policy that also scopes by tenant, which under the permissive model ORed
// with it.
func TestMigration0030HandlesATableWithTwoTenantScopedPermissivePolicies(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the two-boundary case")
	}
	ctx := context.Background()
	admin, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.m30two_doc CASCADE`)
	}
	drop()
	t.Cleanup(drop)

	// Two permissive policies, both scoping by tenant, as the old model allowed.
	if _, err := admin.Exec(ctx, `
CREATE TABLE atlantis.m30two_doc (id int PRIMARY KEY, tenant text NOT NULL, deleted_at timestamptz);
INSERT INTO atlantis.m30two_doc VALUES (1,'acme',NULL),(2,'globex',NULL),(3,'acme',now());
ALTER TABLE atlantis.m30two_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.m30two_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY m30two_doc_tenant_isolation ON atlantis.m30two_doc
  AS PERMISSIVE USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY m30two_doc_operator_scope ON atlantis.m30two_doc
  AS PERMISSIVE USING (tenant = atlantis.current_partition() AND deleted_at IS NULL)
  WITH CHECK (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	runMigration(t, admin, "0030_convert_remaining_permissive_boundaries.up.sql")

	// Every policy that calls current_partition() must have moved to the
	// restrictive slot. One left permissive is a boundary any later grant ORs
	// past, which is the whole condition 0030 exists to remove.
	rows, err := admin.Query(ctx, `
SELECT p.polname, p.polpermissive
  FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = 'm30two_doc' ORDER BY p.polname`)
	if err != nil {
		t.Fatalf("read pg_policy: %v", err)
	}
	defer rows.Close()
	perm, restr := []string{}, []string{}
	for rows.Next() {
		var n string
		var p bool
		if err := rows.Scan(&n, &p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if p {
			perm = append(perm, n)
		} else {
			restr = append(restr, n)
		}
	}
	for _, want := range []string{"m30two_doc_tenant_isolation", "m30two_doc_operator_scope"} {
		found := false
		for _, got := range restr {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not restrictive after the migration. restrictive=%v permissive=%v",
				want, restr, perm)
		}
	}
	// No `_legacy` policy may survive. One left behind means an iteration
	// aborted between its rename and its drop.
	for _, n := range append(append([]string{}, perm...), restr...) {
		if strings.Contains(n, "_legacy") {
			t.Errorf("a %s policy survived the migration, so an iteration did not "+
				"complete and the table is in a half-converted state", n)
		}
	}
	if len(perm) == 0 {
		t.Fatal("no permissive policy survives, so this table denies every row to " +
			"every caller. Restrictive policies only ever subtract")
	}
}
