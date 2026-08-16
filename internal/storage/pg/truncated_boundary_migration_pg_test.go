package pg

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Migration 0030 converts the boundaries 0025 selected past.
//
// 0025 matched `polname LIKE '%\_tenant\_isolation'`. codegen derives that name
// through truncateIdent, which replaces everything past 54 bytes with a hash
// suffix — so a table name of 47 bytes or more yields a policy called
// `..._snapshot_te_6d29d49d`, and the LIKE does not match it. 0025 walked past
// the table and reported success, leaving the boundary PERMISSIVE on precisely
// the tables where emitForeignPolicyGuard is no longer there to refuse a second
// permissive policy.
//
// # The fixture is not hand-named
//
// The entity is declared and run through codegen.EmitInitial, so the policy
// name is whatever partitionPolicyName actually produces. Hard-coding a
// truncated string would prove the migration handles a name I invented; this
// proves it handles the name atlantis emits. The premise check below asserts
// the name really is outside 0025's pattern, so the test cannot quietly stop
// covering the case it exists for.
//
// # Why the real .sql runs
//
// Same reason the 0025 test gives: the migration is a DO block of catalogue
// lookups and format() calls, and the file is what ships. A Go re-implementation
// would be a second thing to get wrong.

const rollupTable = "analytics_customer_engagement_daily_rollup_snapshot"

// longEntity is the declaration whose emitted policy name truncates.
func longEntity() *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name:           "CustomerEngagementDailyRollupSnapshot",
		Namespace:      "analytics",
		PartitionField: "tenant",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "tenant", Type: dsl.FieldType{Name: "varchar", Len: 16}, NotNull: true},
			{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}}}
}

// boundaryState reports the name, permissiveness and predicate of the policy
// that calls current_partition() on the fixture table.
func boundaryState(t *testing.T, db *Pool) (name string, permissive bool, qual string) {
	t.Helper()
	err := db.QueryRow(context.Background(), `
SELECT p.polname, p.polpermissive, pg_get_expr(p.polqual, p.polrelid)
  FROM pg_policy p
  JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = $1
   AND EXISTS (SELECT 1 FROM pg_depend d
                WHERE d.classid = 'pg_policy'::regclass AND d.objid = p.oid
                  AND d.refclassid = 'pg_proc'::regclass
                  AND d.refobjid = to_regprocedure('atlantis.current_partition()'))`,
		rollupTable).Scan(&name, &permissive, &qual)
	if err != nil {
		t.Fatalf("read the boundary on %s: %v", rollupTable, err)
	}
	return name, permissive, qual
}

func permissiveNames(t *testing.T, db *Pool) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
SELECT p.polname FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = $1 AND p.polpermissive ORDER BY p.polname`, rollupTable)
	if err != nil {
		t.Fatalf("list permissive policies: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// preZeroTwentyFive builds the table and puts its boundary back in the
// permissive slot, which is the shape a database carried before 0025.
//
// It cannot get there by running 0025's DOWN migration: that selects by the
// same LIKE pattern and would miss this table for the same reason the up half
// does. So the boundary is read out of the catalogue by dependency, dropped,
// and re-created permissive under its own name — with an operator's own
// `deleted_at IS NULL` hardening added, because carrying the predicate verbatim
// is the second thing this migration has to get right and a regenerated
// predicate would silently drop it.
//
// It also returns the grant name CODEGEN chose, so the migration's own
// derivation can be checked against it. Both replicate truncateIdent — Go's on
// bytes, the migration's in plpgsql — and if they ever disagree the operator is
// told in the docs to drop a policy name that is not the one on their table.
func preZeroTwentyFive(t *testing.T, dsn string) (*Pool, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.`+rollupTable+` CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY m30t_probe`, `DROP ROLE IF EXISTS m30t_probe`)
	}
	drop()
	t.Cleanup(drop)

	initial, err := codegen.EmitInitial(longEntity())
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v\n%s", err, initial.Up)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO atlantis.`+rollupTable+` VALUES
  (1,'acme',NULL,'acme-live'), (2,'globex',NULL,'globex-live'), (3,'acme',now(),'acme-deleted')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	name, permissive, _ := boundaryState(t, admin)
	if permissive {
		t.Fatalf("codegen emitted the boundary %q as PERMISSIVE; this fixture "+
			"exists to put it back to that state and there is nothing to undo", name)
	}
	// The premise. If truncateIdent ever stops firing for this entity the whole
	// test degrades into a re-run of the 0025 case, silently.
	if strings.HasSuffix(name, "_tenant_isolation") {
		t.Fatalf("the emitted policy name %q matches 0025's LIKE pattern, so this "+
			"fixture no longer covers the truncated-name case that 0030 exists for",
			name)
	}

	// codegen's own grant name, captured before the fixture strips it.
	codegenGrant := ""
	for _, p := range permissiveNames(t, admin) {
		codegenGrant = p
	}
	if codegenGrant == "" {
		t.Fatal("codegen emitted no permissive grant, so the table it just created " +
			"denies every row and there is no name to compare against")
	}

	// Down to the pre-0025 shape: one permissive policy, hardened by the operator.
	if _, err := admin.Exec(ctx, `
DROP POLICY `+pgx.Identifier{name}.Sanitize()+` ON atlantis.`+rollupTable+`;
CREATE POLICY `+pgx.Identifier{name}.Sanitize()+` ON atlantis.`+rollupTable+`
  AS PERMISSIVE
  USING (deleted_at IS NULL AND tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("put the boundary back in the permissive slot: %v", err)
	}
	for _, p := range permissiveNames(t, admin) {
		if p == name {
			continue
		}
		if _, err := admin.Exec(ctx,
			`DROP POLICY `+pgx.Identifier{p}.Sanitize()+` ON atlantis.`+rollupTable); err != nil {
			t.Fatalf("drop %s: %v", p, err)
		}
	}
	return admin, codegenGrant
}

func runMigration(t *testing.T, db *Pool, file string) {
	t.Helper()
	sql, err := os.ReadFile("../../../migrations/infra/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := db.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("run %s: %v", file, err)
	}
}

func TestMigration0030ConvertsABoundaryWhoseNameWasTruncated(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the truncated-name migration")
	}
	ctx := context.Background()
	admin, codegenGrant := preZeroTwentyFive(t, dsn)

	// Both, in order, because that is what a real database runs. 0025 is
	// expected to walk past this table; nothing here asserts that it does, so
	// fixing 0025 as well would not break this test — 0030 simply finds nothing
	// left to do.
	runMigration(t, admin, "0025_restrictive_tenant_policy.up.sql")
	runMigration(t, admin, "0030_convert_remaining_permissive_boundaries.up.sql")

	name, permissive, qual := boundaryState(t, admin)
	if permissive {
		t.Fatalf("the boundary %q is still PERMISSIVE after both migrations. "+
			"Permissive policies OR, and emitForeignPolicyGuard — the check that "+
			"used to refuse a second permissive policy — was deleted on the "+
			"strength of 0025 having done this. One access-control grant on this "+
			"table now reads every tenant", name)
	}
	if !strings.Contains(qual, "deleted_at") {
		t.Errorf("the migration regenerated the predicate instead of carrying it: %q. "+
			"The operator's `deleted_at IS NULL` clause is gone, so soft-deleted "+
			"rows are visible to every caller and the migration reported success", qual)
	}
	// The grant the migration created must carry the name codegen would give it.
	// Both derive it through truncateIdent — Go's over bytes, the migration's in
	// plpgsql over characters — and this fixture is chosen so the derivation
	// actually fires. If the two ever disagree, every doc page telling an
	// operator to drop `<table>_default_access` names a policy that is not on
	// their table, and the two agreeing is not something reading either one
	// tells you.
	if got := permissiveNames(t, admin); len(got) != 1 || got[0] != codegenGrant {
		t.Fatalf("the permissive policies are %v, want exactly [%s].\n"+
			"  empty        → the table denies every row to every caller, because "+
			"restrictive policies only subtract\n"+
			"  a other name → the migration's plpgsql truncateIdent disagrees with "+
			"codegen's, so the documented policy name is wrong on long tables",
			got, codegenGrant)
	}

	// Running it again must be a no-op, because a repair migration is the kind
	// an operator re-runs. A second pass that re-selected the now-restrictive
	// boundary would rename and rebuild it forever.
	t.Run("idempotent", func(t *testing.T) {
		before := permissiveNames(t, admin)
		runMigration(t, admin, "0030_convert_remaining_permissive_boundaries.up.sql")
		n2, p2, _ := boundaryState(t, admin)
		if n2 != name || p2 {
			t.Errorf("a second run changed the boundary from %q (restrictive) to "+
				"%q (permissive=%v)", name, n2, p2)
		}
		if after := permissiveNames(t, admin); strings.Join(after, ",") != strings.Join(before, ",") {
			t.Errorf("a second run changed the permissive policies from %v to %v",
				before, after)
		}
	})

	// And the consequence, through a role the policies apply to, with the
	// hostile grant the old model had to refuse outright.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE m30t_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO m30t_probe;
GRANT SELECT ON atlantis.`+rollupTable+` TO m30t_probe;`)
		return err
	})
	if _, err := admin.Exec(ctx,
		`ALTER TABLE atlantis.`+rollupTable+` OWNER TO m30t_probe`); err != nil {
		t.Fatalf("hand the table over: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE POLICY m30t_user_rbac ON atlantis.`+rollupTable+`
  AS PERMISSIVE USING (true) WITH CHECK (true)`); err != nil {
		t.Fatalf("add the user grant: %v", err)
	}

	probe, err := New(ctx, DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//m30t_probe:probe@", 1)))
	if err != nil {
		t.Fatalf("connect as the probe role: %v", err)
	}
	t.Cleanup(probe.Close)

	tx, err := probe.BeginTx(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition('acme')`); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var ids string
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(string_agg(id::text, ',' ORDER BY id), '') FROM atlantis.`+rollupTable).Scan(&ids); err != nil {
		t.Fatalf("bound read: %v", err)
	}
	// 2 is globex's. 3 is acme's but soft-deleted, and only the carried clause
	// excludes it — so this distinguishes "boundary moved" from "boundary
	// rewritten", which the catalogue check above cannot.
	if ids != "1" {
		t.Errorf("a caller bound to acme read %q, want \"1\".\n"+
			"  2 present → `USING (true)` crossed the tenant boundary\n"+
			"  3 present → the carried `deleted_at IS NULL` clause was lost", ids)
	}
}

// A table that already carries the operator's own grants must not acquire a
// `USING (true)` one.
//
// 0025 created `<table>_default_access` unconditionally. Permissive policies OR,
// so adding a total grant beside narrow ones does not add a policy — it stops
// every narrow one from constraining anything, while the catalogue still lists
// them exactly as written. 0030 creates the grant only when dropping the legacy
// boundary would otherwise leave the table admitting nothing.
func TestMigration0030DoesNotWidenATableThatAlreadyHasGrants(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the truncated-name migration")
	}
	ctx := context.Background()
	admin, _ := preZeroTwentyFive(t, dsn)

	if _, err := admin.Exec(ctx, `CREATE POLICY m30t_live_only ON atlantis.`+rollupTable+`
  AS PERMISSIVE USING (deleted_at IS NULL) WITH CHECK (deleted_at IS NULL)`); err != nil {
		t.Fatalf("add the operator's grant: %v", err)
	}

	runMigration(t, admin, "0030_convert_remaining_permissive_boundaries.up.sql")

	got := permissiveNames(t, admin)
	if len(got) != 1 || got[0] != "m30t_live_only" {
		t.Errorf("the permissive policies are %v, want only m30t_live_only. "+
			"A total grant beside a narrow one makes the narrow one stop "+
			"constraining anything", got)
	}
	if _, permissive, _ := boundaryState(t, admin); permissive {
		t.Error("the boundary is still permissive, so the table was never converted " +
			"and the check above passed for the wrong reason")
	}
}
