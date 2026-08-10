package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Executes the RLS the emitter generates for `partition by`, then attacks it
// as a tenant role, on a live PostgreSQL.
//
// This is the only kind of test that can support the claim. The previous
// implementation had a golden test asserting the emitter wrote a predicate into
// a generated file, and it stayed green for the entire period during which
// every read returned every tenant's rows — because the file it asserted on was
// not compiled. A string is not isolation. Reading as one tenant and failing to
// see another's row is.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/codegen/ -run PartitionRLS -v

func partitionIR() *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Doc", Namespace: "rlst",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "tenant_id", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
		PartitionField: "tenant_id",
	}}}
}

func TestPartitionRLSIsolatesTenantsAgainstHostileSQL(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the generated RLS")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup, not defer: deferred closes run when the test function
	// returns, which is before any t.Cleanup registered later, so a cleanup
	// that drops objects through this connection would run against a closed
	// one and fail silently. That is how atlantis.rlst_doc and the rls_tenant
	// role came to survive test runs. Cleanups run last-registered-first, so
	// registering the close here puts it after the drops below.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	// The migration this design lives in must be present, or every assertion
	// below would pass by the policy erroring rather than by isolating.
	var haveFn bool
	if err := admin.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'atlantis' AND p.proname = 'current_partition')`).Scan(&haveFn); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !haveFn {
		t.Skip("migration 0021 not applied to this database")
	}

	cleanup := func() {
		_, _ = admin.Exec(ctx, `DROP TABLE IF EXISTS atlantis.rlst_doc CASCADE`)
		_, _ = admin.Exec(ctx, `DROP OWNED BY rls_tenant`)
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS rls_tenant`)
	}
	cleanup()
	t.Cleanup(cleanup)

	scripts, err := EmitInitial(partitionIR())
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	// The generated DDL must carry the policy; if it does not, everything below
	// would "pass" by there being no security to defeat.
	for _, want := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY", // without FORCE the owner bypasses it
		"atlantis.current_partition()",
		"WITH CHECK", // write side, not only read
	} {
		if !strings.Contains(scripts.Up, want) {
			t.Fatalf("generated DDL is missing %q:\n%s", want, scripts.Up)
		}
	}
	// The policy must go through atlantis.current_partition() and must not
	// inline whatever that function currently reads.
	//
	// The indirection is what let migration 0024 move the discriminator from a
	// table to a run-time parameter without touching a single policy. That
	// matters more than it sounds: emitPartitionPolicy runs on CREATE TABLE
	// only and no differ reads `partition by` (task #36), so a policy whose
	// text had to change would be a policy on every existing partitioned table
	// that nothing can migrate.
	if strings.Contains(scripts.Up, "current_setting(") {
		t.Errorf("the policy inlines current_setting() instead of calling "+
			"atlantis.current_partition(). The mechanism then lives in every "+
			"emitted policy, and policies are emitted once at CREATE TABLE with "+
			"no differ that can revisit them:\n%s", scripts.Up)
	}
	if _, err := admin.Exec(ctx, scripts.Up); err != nil {
		t.Fatalf("generated DDL did not apply:\n%s\n  %v", scripts.Up, err)
	}

	// Seed as the owner. FORCE applies to the owner too, so the partition has
	// to be set even here — which is itself worth asserting.
	if _, err := admin.Exec(ctx, `
BEGIN;
SELECT atlantis.set_partition('acme');
INSERT INTO atlantis.rlst_doc VALUES (1,'acme','acme-secret');
COMMIT;`); err != nil {
		t.Fatalf("seed acme: %v", err)
	}
	if _, err := admin.Exec(ctx, `
BEGIN;
SELECT atlantis.set_partition('victim');
INSERT INTO atlantis.rlst_doc VALUES (2,'victim','victim-secret');
COMMIT;`); err != nil {
		t.Fatalf("seed victim: %v", err)
	}

	// A tenant role with the privileges a caller actually has: no superuser, no
	// ownership, SELECT and INSERT on the table.
	if _, err := admin.Exec(ctx, `
CREATE ROLE rls_tenant LOGIN PASSWORD 'probe' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO rls_tenant;
GRANT SELECT, INSERT ON atlantis.rlst_doc TO rls_tenant;`); err != nil {
		t.Fatalf("create tenant role: %v", err)
	}

	// Parsed, not substituted. See replaceUserInfo — the substitution this
	// replaced returned the URL unchanged for any DSN not spelled exactly
	// "atlantis:atlantis@", and every assertion below then ran as the admin
	// role, which row-level security does not apply to.
	tenantURL := replaceUserInfo(url, "rls_tenant:probe")
	conn, err := pgx.Connect(ctx, tenantURL)
	if err != nil {
		t.Fatalf("connect as tenant: %v", err)
	}
	defer conn.Close(ctx)

	// Statements are run one at a time on the same connection. pgx prepares
	// statements, so a multi-statement string is rejected outright — which is
	// itself the reality for a caller-authored query body: it reaches Postgres
	// as a single statement and cannot contain BEGIN, SAVEPOINT or COMMIT.
	//
	// Running them separately therefore grants the attacker MORE than the real
	// execution path does. If isolation holds here it holds there.
	// Stops at the FIRST error and reports it.
	//
	// Continuing past one is what makes this kind of test vacuous: the next
	// statement fails with "current transaction is aborted", that cascade
	// becomes the observed result, and an assertion checking only that the
	// victim's data is absent then passes for any reason at all — including
	// reasons unrelated to isolation. The refusal has to be attributable.
	read := func(t *testing.T, script string) string {
		t.Helper()
		var last string
		for _, stmt := range strings.Split(script, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if strings.HasPrefix(strings.ToUpper(stmt), "SELECT COALESCE") {
				if err := conn.QueryRow(ctx, stmt).Scan(&last); err != nil {
					return "ERROR: " + err.Error()
				}
				return last
			}
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return "ERROR: " + err.Error()
			}
		}
		return last
	}

	// Baseline: the legitimate flow sees its own row and only its own row.
	if got := read(t, `
BEGIN;
SELECT atlantis.set_partition('acme');
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`); got != "acme-secret" {
		t.Fatalf("legitimate read returned %q, want acme-secret", got)
	}
	_, _ = conn.Exec(ctx, "COMMIT")

	// Every attack below is caller-authored SQL of the kind atlantis executes:
	// custom-query bodies, CHECK expressions, backfill expressions.
	// wantRefusal is the substring the refusal must contain, or "" when the
	// attack is expected to reach the read and come back correctly scoped.
	// Three outcomes, not two, and the third is the honest one.
	//
	// Migration 0024 moved the discriminator into a run-time parameter, and
	// PostgreSQL will not lock a custom parameter — `REVOKE SET ON PARAMETER`
	// does not create so much as a pg_parameter_acl row for a placeholder GUC,
	// re-verified on 17.8. So one attack below DOES breach when the database is
	// the only thing between it and the data.
	//
	// It is asserted as a breach rather than deleted. A suite that only lists
	// what the database stops reads as though the database stops everything,
	// and the layer that actually stops this one — sqlvalidate, see
	// TestValidateCustomQuery_RejectsSetConfig — would then have nothing here
	// pointing at it. If PostgreSQL ever gains a way to lock the parameter,
	// this case fails and whoever sees it can move the defence down a layer.
	for _, tc := range []struct {
		name        string
		script      string
		wantRead    string
		wantRefusal string
		// wantDBLeak names the layer that stops an attack the database does
		// not. Set only where the breach is real and deliberate.
		wantDBLeak string
	}{
		{
			name: "reassign the parameter mid-transaction",
			wantDBLeak: "sqlvalidate rejects set_config in caller SQL " +
				"(TestValidateCustomQuery_RejectsSetConfig)",
			script: `
BEGIN;
SELECT atlantis.set_partition('acme');
SELECT set_config('atlantis.tenant','victim',true);
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
		{
			name:        "call the setter again with another tenant",
			wantRefusal: "partition already set",
			script: `
BEGIN;
SELECT atlantis.set_partition('acme');
SELECT atlantis.set_partition('victim');
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
		{
			// The setter's guard reads the live parameter, so a savepoint that
			// unwinds the SET also unwinds the guard's view of it. Both are
			// transaction-scoped, so they cannot disagree.
			name:        "escape the once-only guard through a savepoint",
			wantRefusal: "partition already set",
			script: `
BEGIN;
SELECT atlantis.set_partition('acme');
SAVEPOINT s1;
SELECT atlantis.set_partition('victim');
ROLLBACK TO s1;
SELECT atlantis.set_partition('victim');
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
		{
			name:        "bind with an empty tenant",
			wantRefusal: "non-empty",
			script: `
BEGIN;
SELECT atlantis.set_partition('');
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
		{
			name:     "read with no partition set at all",
			wantRead: "(none)",
			script: `
BEGIN;
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
		{
			// The pooling case, and the reason set_partition passes `true`: a
			// transaction-local parameter reverts when its transaction ends, so
			// the next request on this connection starts unbound.
			//
			// It does NOT cover the nullif in current_partition(), though an
			// earlier version of this comment claimed it did. The revert lands
			// on the empty string rather than NULL, but rlst_doc holds no row
			// whose tenant is '', so `tenant = ''` and `tenant = NULL` both
			// match nothing here — removing the nullif leaves this subtest
			// green. TestPartitionDiscriminatorNeverReturnsEmptyString seeds
			// exactly that row and is what makes the property fail when broken;
			// TestTenantResetAtConnect catches it too.
			name:     "a bind does not survive its own transaction",
			wantRead: "(none)",
			script: `
BEGIN;
SELECT atlantis.set_partition('acme');
COMMIT;
BEGIN;
SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.rlst_doc;`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := read(t, tc.script)
			_, _ = conn.Exec(ctx, "ROLLBACK")

			if tc.wantDBLeak != "" {
				// Assert the breach happens. If it stops happening, the comment
				// above this table is out of date and somebody should know.
				if !strings.Contains(got, "victim-secret") {
					t.Errorf("this attack no longer reaches the victim's rows (got %q). "+
						"PostgreSQL alone was not expected to stop it — the defence "+
						"is %s. If the database now blocks it, move the defence down "+
						"and delete this case.", got, tc.wantDBLeak)
				}
				return
			}

			if strings.Contains(got, "victim-secret") {
				t.Fatalf("CROSS-TENANT LEAK: %q. The attack is ordinary SQL a caller "+
					"can put in a custom query body, a CHECK expression or a backfill "+
					"expression — all of which atlantis executes on this connection.", got)
			}
			switch {
			case tc.wantRefusal != "":
				// The attack must be refused, and refused for the stated reason.
				// Accepting any error would accept "current transaction is
				// aborted", which says nothing about isolation.
				if !strings.Contains(got, tc.wantRefusal) {
					t.Errorf("refusal = %q, want it to contain %q. Absence of the "+
						"victim's data is not evidence on its own — the refusal has "+
						"to be attributable to the guard that produced it.",
						got, tc.wantRefusal)
				}
			default:
				// The attack must REACH the read and come back correctly scoped.
				// An error here would mean the test never observed isolation.
				if got != tc.wantRead {
					t.Errorf("read = %q, want %q. The attack has to reach the query "+
						"for this case to demonstrate anything.", got, tc.wantRead)
				}
			}
		})
	}

	// The write side. USING alone would let a caller INSERT a row attributed to
	// another tenant: invisible to itself afterwards, but planted in the
	// victim's data.
	t.Run("cannot write into another tenant", func(t *testing.T) {
		_, _ = conn.Exec(ctx, "ROLLBACK")
		_, err := conn.Exec(ctx, `
BEGIN;
SELECT atlantis.set_partition('acme');
INSERT INTO atlantis.rlst_doc VALUES (99,'victim','planted');`)
		_, _ = conn.Exec(ctx, "ROLLBACK")
		if err == nil {
			t.Error("a tenant inserted a row attributed to another tenant; the policy " +
				"needs WITH CHECK, not only USING")
		}
	})
}

// Demonstrates that a GUC-based policy leaks when nothing but PostgreSQL
// stands behind it.
//
// It builds its OWN policy on its OWN parameter — atlantis.partition, USING
// only, never touching atlantis.current_partition() — so it is a statement
// about PostgreSQL, not about the shipped mechanism. That distinction was got
// wrong once already: an earlier version of this comment claimed the test
// executed a property of production, and a reviewer disproved it by replacing
// atlantis.current_partition() with a constant, neutering the real mechanism
// entirely, while this test carried on passing.
//
// What it does establish is the premise everything else rests on: PostgreSQL
// alone does not stop caller SQL from rebinding a custom parameter, and cannot
// be made to — a custom GUC is PGC_USERSET and `REVOKE SET ON PARAMETER` does
// not create so much as an ACL row for one.
//
// What stops it is internal/dsl/sqlvalidate, which refuses set_config and
// set_partition in every caller-authored SQL surface: query bodies, procedure
// steps, CHECK expressions and partial-index predicates. This test is the
// standing evidence for why that validator may not be weakened or narrowed —
// the first version of it covered only query bodies, and an adversarial review
// walked a set_config through a CHECK expression in minutes.
//
// If PostgreSQL ever changes so that a custom GUC can be locked, this test
// fails — and that failure is the signal that the defence can move down a
// layer, into the database where it belongs.
func TestTheGUCBasedPolicyLeaksAcrossTenants(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to demonstrate the GUC leak")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup, not defer: deferred closes run when the test function
	// returns, which is before any t.Cleanup registered later, so a cleanup
	// that drops objects through this connection would run against a closed
	// one and fail silently. That is how atlantis.rlst_doc and the rls_tenant
	// role came to survive test runs. Cleanups run last-registered-first, so
	// registering the close here puts it after the drops below.
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	cleanup := func() {
		_, _ = admin.Exec(ctx, `DROP TABLE IF EXISTS atlantis.guc_doc CASCADE`)
		_, _ = admin.Exec(ctx, `DROP OWNED BY guc_tenant`)
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS guc_tenant`)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err := admin.Exec(ctx, `
CREATE TABLE atlantis.guc_doc (id int PRIMARY KEY, tenant_id text NOT NULL, body text);
INSERT INTO atlantis.guc_doc VALUES (1,'acme','acme-secret'),(2,'victim','victim-secret');
ALTER TABLE atlantis.guc_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.guc_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON atlantis.guc_doc
  USING (tenant_id = current_setting('atlantis.partition', true));
CREATE ROLE guc_tenant LOGIN PASSWORD 'probe' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT USAGE ON SCHEMA atlantis TO guc_tenant;
GRANT SELECT ON atlantis.guc_doc TO guc_tenant;`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Parsed, not substituted — see replaceUserInfo. The Replace form this
	// used returned the URL unchanged for any DSN not spelled exactly
	// "atlantis:atlantis@", which connected as the admin role instead. A test
	// whose whole job is to demonstrate a leak would then have demonstrated it
	// as a superuser, for whom row-level security never applies, and passed.
	conn, err := pgx.Connect(ctx, replaceUserInfo(url, "guc_tenant:probe"))
	if err != nil {
		t.Fatalf("connect as tenant: %v", err)
	}
	defer conn.Close(ctx)

	// The intended flow first, so a failure below cannot be blamed on the
	// policy simply not working.
	if _, err := conn.Exec(ctx, `SET atlantis.partition = 'acme'`); err != nil {
		t.Fatalf("set: %v", err)
	}
	var legit string
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.guc_doc`).Scan(&legit); err != nil {
		t.Fatalf("legit read: %v", err)
	}
	if legit != "acme-secret" {
		t.Fatalf("the GUC policy does not work at all (got %q); this test cannot "+
			"demonstrate anything about it", legit)
	}

	// One statement of ordinary SQL — the kind that goes in a custom query
	// body, a CHECK expression or a backfill expression.
	if _, err := conn.Exec(ctx,
		`SELECT set_config('atlantis.partition','victim',false)`); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	var after string
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(string_agg(body, ','), '(none)') FROM atlantis.guc_doc`).Scan(&after); err != nil {
		t.Fatalf("post-attack read: %v", err)
	}

	if !strings.Contains(after, "victim-secret") {
		t.Errorf("the parameter-based policy did NOT leak (got %q).\n"+
			"  Check the fixture first: if this connected as a role row-level "+
			"security does not apply to, the read proves nothing. If it is a "+
			"genuine PostgreSQL behaviour change, then a custom parameter can "+
			"now be locked, and the sqlvalidate gate that currently carries this "+
			"defence could move into the database instead.", after)
	}
}
