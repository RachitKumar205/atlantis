package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The default grant belongs to the operator once they replace it, and an
// atlantis migration must not hand it back.
//
// `<table>_default_access` is documented — in partitionDefaultPolicyName's own
// doc comment and in the reference docs — as the policy an operator drops when
// they define their own access control. An emitPartitionPolicy that dropped and
// re-created it unconditionally would restore `USING (true)` on the next apply.
// Permissive policies OR, so that does not put a policy back beside the
// operator's narrow grants: it makes every one of them stop constraining
// anything, while pg_policy still lists them all exactly as written.
//
// This function runs from diffPartition when the clause is added, and from the
// partition-rebuild bracket at the end of any migration that moves the
// discriminator column, so a change classified ADDITIVE is enough to undo an
// access-control model.
//
// Behavioural rather than a string assertion on the emitted DDL, because the
// failure is about what a bound caller can READ afterwards — a check on the SQL
// text would keep passing if the catalogue condition were subtly wrong.

// grantFixture builds the partitioned table, seeds it, and hands back an admin
// pool. Rows are chosen so a single later read distinguishes all three ways
// this can fail; see the assertion in the first test.
func grantFixture(t *testing.T, dsn string) (*pg.Pool, *dsl.IR) {
	t.Helper()
	ctx := context.Background()
	admin, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)

	drop := func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.pgrant_doc CASCADE`)
		pgcatalog.Exec(t, dsn, `DROP OWNED BY pgrant_probe`, `DROP ROLE IF EXISTS pgrant_probe`)
	}
	drop()
	t.Cleanup(drop)

	ir := lower(t, `
entity Doc in pgrant {
  id     bigint primary
  tenant varchar(16) not null
  region varchar(16) not null
  body   text
  partition by tenant
}
`)
	AssignProtoNumbers(nil, ir)
	initial, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("apply initial: %v\n%s", err, initial.Up)
	}
	if _, err := admin.Exec(ctx, `
INSERT INTO atlantis.pgrant_doc VALUES
  (1,'acme','eu','acme-eu'), (2,'acme','us','acme-us'), (3,'globex','eu','globex-eu')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return admin, ir
}

// permissivePolicies lists the permissive policy names on the fixture table.
func permissivePolicies(t *testing.T, db *pg.Pool) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
SELECT polname FROM pg_policy
 WHERE polrelid = 'atlantis.pgrant_doc'::regclass AND polpermissive
 ORDER BY polname`)
	if err != nil {
		t.Fatalf("read pg_policy: %v", err)
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

func TestAnApplyDoesNotRestoreTheGrantAnOperatorReplaced(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the default grant")
	}
	ctx := context.Background()
	admin, ir := grantFixture(t, dsn)

	// The premise. Without this the rest of the test would pass against a
	// version that never emitted a default grant at all, which is a different
	// bug and not the one being pinned.
	if got := permissivePolicies(t, admin); len(got) != 1 || got[0] != "pgrant_doc_default_access" {
		t.Fatalf("the fixture did not start with the default grant in place, so "+
			"nothing below is testing a replacement: %v", got)
	}

	// The documented path: drop the total grant, write a narrower one.
	if _, err := admin.Exec(ctx, `
DROP POLICY pgrant_doc_default_access ON atlantis.pgrant_doc;
CREATE POLICY pgrant_doc_eu_only ON atlantis.pgrant_doc
  AS PERMISSIVE USING (region = 'eu') WITH CHECK (region = 'eu');`); err != nil {
		t.Fatalf("replace the default grant: %v", err)
	}

	// Two re-invocations, because they reach this function by different routes
	// and only one of them looks like "creating an entity".
	t.Run("re-applying the initial script", func(t *testing.T) {
		initial, err := EmitInitial(ir)
		if err != nil {
			t.Fatalf("EmitInitial: %v", err)
		}
		if _, err := admin.Exec(ctx, initial.Up); err != nil {
			t.Fatalf("re-apply: %v", err)
		}
	})

	t.Run("a migration that moves the discriminator column", func(t *testing.T) {
		after := lower(t, `
entity Doc in pgrant {
  id     bigint primary
  tenant varchar(16) not null
  region varchar(16) not null
  body   text
  partition by region
}
`)
		AssignProtoNumbers(ir, after)
		scripts, err := EmitSQL(ir, after, ComputeDiff(ir, after))
		if err != nil {
			t.Fatalf("EmitSQL: %v", err)
		}
		// Rolled back: the rest of the test asserts against `partition by
		// tenant`. Running it inside a transaction still executes every
		// statement, so the DO block either creates the grant or does not.
		tx, err := admin.BeginTx(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, err := tx.Exec(ctx, scripts.Up); err != nil {
			t.Fatalf("apply the column move: %v\n%s", err, scripts.Up)
		}
		var n int
		if err := tx.QueryRow(ctx, `
SELECT count(*) FROM pg_policy
 WHERE polrelid = 'atlantis.pgrant_doc'::regclass
   AND polpermissive AND polname = 'pgrant_doc_default_access'`).Scan(&n); err != nil {
			t.Fatalf("read pg_policy: %v", err)
		}
		if n != 0 {
			t.Error("moving the partition column re-created the total grant the " +
				"operator had replaced. This arrives through diffPartition, not " +
				"through entity creation, and the migration carrying it need not " +
				"be classified any worse than additive")
		}
	})

	if got := permissivePolicies(t, admin); len(got) != 1 || got[0] != "pgrant_doc_eu_only" {
		t.Fatalf("after the apply the permissive policies are %v, want only "+
			"pgrant_doc_eu_only. Restoring `USING (true)` beside a narrow grant "+
			"does not add a policy — permissive policies OR, so it makes the "+
			"narrow one stop constraining anything", got)
	}

	// And the consequence, read through a role the policies actually apply to.
	pgcatalog.Do(t, dsn, func(conn *pgx.Conn) error {
		_, err := conn.Exec(context.Background(), `
CREATE ROLE pgrant_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO pgrant_probe;
GRANT SELECT ON atlantis.pgrant_doc TO pgrant_probe;`)
		return err
	})
	if _, err := admin.Exec(ctx, `ALTER TABLE atlantis.pgrant_doc OWNER TO pgrant_probe`); err != nil {
		t.Fatalf("hand the table over: %v", err)
	}
	probe, err := pg.New(ctx, pg.DefaultConfig(
		strings.Replace(dsn, "//atlantis:atlantis@", "//pgrant_probe:probe@", 1)))
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
	var bodies string
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(string_agg(body, ',' ORDER BY id), '') FROM atlantis.pgrant_doc`).Scan(&bodies); err != nil {
		t.Fatalf("read: %v", err)
	}
	// One assertion, three distinguishable failures. acme-us is visible only if
	// the total grant came back; globex-eu is visible only if the boundary did
	// not, which is what stops this test from passing on a build where the
	// conditional swallowed the restrictive policy too.
	if bodies != "acme-eu" {
		t.Errorf("a caller bound to acme read %q, want \"acme-eu\".\n"+
			"  acme-us present  → the apply restored `USING (true)` and the "+
			"operator's region grant no longer constrains anything\n"+
			"  globex-eu present → the tenant boundary is missing", bodies)
	}
}

// The other direction. The grant exists so a table carrying only the
// restrictive boundary is not deny-all, and making its creation conditional
// must not turn that off — an operator who drops it and writes nothing has a
// table no role can read, and the next apply is what fixes them.
//
// Without this, "never re-create it" is a passing fix for the test above and a
// silent way to leave a table unreadable.
func TestAnApplyRestoresTheGrantWhenTheTableWouldAdmitNothing(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the default grant")
	}
	ctx := context.Background()
	admin, ir := grantFixture(t, dsn)

	if _, err := admin.Exec(ctx,
		`DROP POLICY pgrant_doc_default_access ON atlantis.pgrant_doc`); err != nil {
		t.Fatalf("drop the default grant: %v", err)
	}
	if got := permissivePolicies(t, admin); len(got) != 0 {
		t.Fatalf("the table still carries a permissive policy, so it is not in "+
			"the deny-all state this test is about: %v", got)
	}

	initial, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	if _, err := admin.Exec(ctx, initial.Up); err != nil {
		t.Fatalf("re-apply: %v", err)
	}

	if got := permissivePolicies(t, admin); len(got) != 1 || got[0] != "pgrant_doc_default_access" {
		t.Errorf("the permissive policies are %v, want pgrant_doc_default_access. "+
			"Restrictive policies only subtract, so a table carrying the boundary "+
			"alone admits nothing at all and every read returns zero rows", got)
	}
}
