package introspect

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Introspection must report the isolation the DATABASE enforces, not the
// isolation the schema file claims.
//
// PartitionField copied off the declared entity makes the live IR agree with
// the schema by construction: ComputeDiff compares a value to itself,
// diffPartition returns on its first line, and `tide adopt` cannot report a
// table whose policy the database is not enforcing.
//
// Strip the policy, FORCE and RLS from a live table whose declaration says
// `partition by tenant`, and that shape reports the column as partitioned with
// no warning and an empty diff.
//
// It is the drift most likely to be present, since a deployment predating the
// differ's policy emission has a schema claiming a partition the database never
// had.
func TestIntrospectReadsIsolationFromTheDatabase(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise partition introspection")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		for _, tbl := range []string{"ipol_enforced", "ipol_nopolicy", "ipol_noforce",
			"ipol_wrongcol", "ipol_forcenotenabled", "ipol_restrictonly", "ipol_deleteonly",
			"ipol_secondperm", "ipol_checktrue", "ipol_toother", "ipol_twocols"} {
			_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.`+tbl+` CASCADE`)
		}
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.ipol_enforced (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_enforced ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_enforced FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_enforced AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_enforced AS PERMISSIVE USING (true) WITH CHECK (true);

CREATE TABLE atlantis.ipol_nopolicy (id bigint primary key, tenant text not null, org text not null);

CREATE TABLE atlantis.ipol_noforce (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_noforce ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_noforce AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_noforce AS PERMISSIVE USING (true) WITH CHECK (true);

CREATE TABLE atlantis.ipol_wrongcol (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_wrongcol ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_wrongcol FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_wrongcol AS RESTRICTIVE
  USING (org = atlantis.current_partition())
  WITH CHECK (org = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_wrongcol AS PERMISSIVE USING (true) WITH CHECK (true);

-- FORCE without ENABLE. PostgreSQL permits this and it leaves the policy
-- inert: FORCE only says the owner is not exempt, ENABLE is what switches
-- row-level security on at all. Dropping the ENABLE check from the probe
-- survived a fixture that lacked this table.
CREATE TABLE atlantis.ipol_forcenotenabled (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_forcenotenabled FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_forcenotenabled AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());

-- RESTRICTIVE only. Restrictive policies AND with the permissive set, and with
-- no permissive policy the table denies every row rather than scoping it. That
-- is not isolation, it is an outage, and reporting it as isolation would let
-- adopt agree with a schema whose table serves nothing.
CREATE TABLE atlantis.ipol_restrictonly (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_restrictonly ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_restrictonly FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_restrictonly AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());

-- FOR DELETE only. This is the shape that tests the polcmd filter, not FOR
-- INSERT: an INSERT policy has no USING clause at all, so pg_get_expr returns
-- nothing and the predicate parser rejects it before polcmd is consulted. A
-- DELETE policy HAS a USING predicate that looks exactly like a correct one,
-- and governs no read. Only '*' (ALL) and 'r' (SELECT) govern reads.
CREATE TABLE atlantis.ipol_deleteonly (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_deleteonly ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_deleteonly FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_deleteonly AS RESTRICTIVE FOR DELETE
  USING (tenant = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_deleteonly AS PERMISSIVE USING (true) WITH CHECK (true);

-- A restrictive boundary beside a wide-open PERMISSIVE policy. This case
-- INVERTED: while the boundary was permissive the pair ORd to USING (true) and
-- the table leaked everything, so this fixture asserted "not isolated". A
-- restrictive boundary ANDs with every grant, so the same shape is now a
-- correctly isolated table carrying a user's access-control rule — and
-- reporting it would refuse every RBAC-carrying deployment.
CREATE TABLE atlantis.ipol_secondperm (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_secondperm ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_secondperm FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_secondperm AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_secondperm AS PERMISSIVE USING (true) WITH CHECK (true);
CREATE POLICY user_rbac ON atlantis.ipol_secondperm FOR SELECT USING (true);

-- Reads scoped, writes wide open. Admits a caller bound to one tenant writing
-- a row stamped with another.
CREATE TABLE atlantis.ipol_checktrue (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_checktrue ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_checktrue FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_checktrue AS RESTRICTIVE
  USING (tenant = atlantis.current_partition()) WITH CHECK (true);
CREATE POLICY g ON atlantis.ipol_checktrue AS PERMISSIVE USING (true) WITH CHECK (true);

-- Granted TO a specific role rather than PUBLIC. It applies to nobody on the
-- connecting role, so with no other policy the table denies every row.
CREATE TABLE atlantis.ipol_toother (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_toother ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_toother FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ipol_toother AS RESTRICTIVE FOR ALL TO pg_monitor
  USING (tenant = atlantis.current_partition()) WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY g ON atlantis.ipol_toother AS PERMISSIVE USING (true) WITH CHECK (true);

-- Two isolation policies on DIFFERENT columns. Last-row-wins over an unordered
-- query made this nondeterministic: four runs said "org", one said "tenant",
-- over five identical calls against an unchanged database.
CREATE TABLE atlantis.ipol_twocols (id bigint primary key, tenant text not null, org text not null);
ALTER TABLE atlantis.ipol_twocols ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ipol_twocols FORCE ROW LEVEL SECURITY;
CREATE POLICY g ON atlantis.ipol_twocols AS PERMISSIVE USING (true) WITH CHECK (true);
CREATE POLICY a_tenant ON atlantis.ipol_twocols AS RESTRICTIVE USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY b_org ON atlantis.ipol_twocols AS RESTRICTIVE USING (org = atlantis.current_partition())
  WITH CHECK (org = atlantis.current_partition());`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// Every entity DECLARES `partition by tenant`. What differs is what the
	// database actually does, which is the whole point.
	declared := &dsl.IR{Entities: []dsl.Entity{
		{Name: "Enforced", Namespace: "ipol", TableName: "atlantis.ipol_enforced", PartitionField: "tenant"},
		{Name: "NoPolicy", Namespace: "ipol", TableName: "atlantis.ipol_nopolicy", PartitionField: "tenant"},
		{Name: "NoForce", Namespace: "ipol", TableName: "atlantis.ipol_noforce", PartitionField: "tenant"},
		{Name: "WrongCol", Namespace: "ipol", TableName: "atlantis.ipol_wrongcol", PartitionField: "tenant"},
		{Name: "ForceNotEnabled", Namespace: "ipol", TableName: "atlantis.ipol_forcenotenabled", PartitionField: "tenant"},
		{Name: "RestrictOnly", Namespace: "ipol", TableName: "atlantis.ipol_restrictonly", PartitionField: "tenant"},
		{Name: "DeleteOnly", Namespace: "ipol", TableName: "atlantis.ipol_deleteonly", PartitionField: "tenant"},
		{Name: "SecondPerm", Namespace: "ipol", TableName: "atlantis.ipol_secondperm", PartitionField: "tenant"},
		{Name: "CheckTrue", Namespace: "ipol", TableName: "atlantis.ipol_checktrue", PartitionField: "tenant"},
		{Name: "ToOther", Namespace: "ipol", TableName: "atlantis.ipol_toother", PartitionField: "tenant"},
		{Name: "TwoCols", Namespace: "ipol", TableName: "atlantis.ipol_twocols", PartitionField: "tenant"},
	}}

	live, _, _, err := FromPostgres(ctx, pool, declared)
	if err != nil {
		t.Fatalf("FromPostgres: %v", err)
	}

	got := map[string]string{}
	for i := range live.Entities {
		got[live.Entities[i].Name] = live.Entities[i].PartitionField
	}

	for _, tc := range []struct {
		entity, want, why string
	}{
		{"Enforced", "tenant",
			"enabled, forced and scoped — the database really does isolate this table"},
		{"NoPolicy", "",
			"the schema claims a partition and no policy exists. This is the modal " +
				"deployment, because no differ emitted the policy until recently"},
		{"NoForce", "",
			"without FORCE the table's owner is exempt, and atlantis owns the tables " +
				"it creates and connects as their owner, so nothing is filtered"},
		{"WrongCol", "org",
			"the policy scopes a column the schema does not declare. Reporting the " +
				"declared name here would call a wrong policy correct"},
		{"ForceNotEnabled", "",
			"FORCE only says the owner is not exempt; ENABLE is what switches " +
				"row-level security on. Without it the policy is inert"},
		{"RestrictOnly", "",
			"restrictive policies AND with the permissive set; with none, the table " +
				"denies every row rather than scoping it, which is an outage and not " +
				"isolation"},
		// INVERTED with the boundary. While it was permissive this pair ORd to
		// USING (true) and the expectation was "". A restrictive boundary ANDs
		// with every grant, so a user's access-control rule sitting beside it is
		// a correctly isolated table — and calling it unisolated would refuse
		// every deployment that defines one.
		{"SecondPerm", "tenant",
			"a wide-open permissive grant cannot cross a restrictive boundary, so " +
				"this is isolated and reporting otherwise breaks user-defined access " +
				"control"},
		{"CheckTrue", "",
			"reads scoped and writes wide open admits a caller bound to one tenant " +
				"writing a row stamped with another"},
		{"ToOther", "",
			"granted TO a role that is not the connecting one, so it applies to " +
				"nobody and the table denies every row"},
		{"TwoCols", "",
			"two isolation policies on different columns. Last-row-wins over an " +
				"unordered query made this answer change between runs against an " +
				"unchanged database"},
		{"DeleteOnly", "",
			"a policy with a USING predicate that governs deletes and not reads leaves " +
				"every SELECT unfiltered, which is the leak this feature exists to prevent"},
	} {
		if got[tc.entity] != tc.want {
			t.Errorf("%s: live PartitionField = %q, want %q — %s",
				tc.entity, got[tc.entity], tc.want, tc.why)
		}
	}
}

// The predicate parser, over the shapes pg_get_expr actually renders.
//
// pg_get_expr strips the schema qualifier and inlines the function body, so the
// text read back bears little resemblance to the text emitted. These are real
// renderings taken from the catalog.
func TestPartitionColumnFromQual(t *testing.T) {
	for _, tc := range []struct {
		qual, want, why string
	}{
		{`(tenant = current_partition())`, "tenant", "the simple rendering"},
		{`((tenant)::text = NULLIF(current_setting('atlantis.tenant'::text, true), ''::text))`,
			"tenant", "the inlined rendering with a cast, which is what a text column gives"},
		{`((org)::text = (current_partition())::text)`, "org", "cast on both sides"},
		{`(true)`, "", "not an isolation policy at all"},
		{`((deleted_at IS NULL))`, "", "an unrelated policy on the same table"},
		{`((a || b) = current_partition())`, "",
			"an expression, not a plain column — reported as not isolated, so the " +
				"plan rebuilds it rather than silently accepting it"},
		{`(t.tenant = current_partition())`, "",
			"a qualified reference is not a plain column"},
		{"", "", "no predicate"},
		// Operands the other way round. Plausible in a hand-written policy, and
		// it used to yield the string "current_partition" as the live partition
		// column — the operator was shown "tenant isolation moved from
		// "current_partition" to "tenant"" and the down script would have
		// created a policy against a column that does not exist.
		{`(current_partition() = tenant)`, "", "a call on the left is not a column"},
		{`((current_partition())::text = (tenant)::text)`, "", "a cast call on the left"},
	} {
		if got := partitionColumnFromQual(tc.qual); got != tc.want {
			t.Errorf("partitionColumnFromQual(%q) = %q, want %q — %s",
				tc.qual, got, tc.want, tc.why)
		}
	}
}
