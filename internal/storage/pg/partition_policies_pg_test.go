package pg

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A partitioned entity whose table carries no policy must be reported.
//
// This is the case a review executed end to end: `partition by` added to an
// entity that already existed, the differ emitted nothing, the server refused
// an unbound request — which looks exactly like isolation working — and a bound
// request returned every tenant's rows. Nothing detected it, because every
// existing boot check asks about the ROLE, and this is a question about the
// TABLE.
func TestVerifyPartitionPolicies(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise policy verification")
	}
	ctx := context.Background()
	pool, err := New(ctx, DefaultConfig(url))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		for _, tbl := range []string{"ppv_none", "ppv_noforce", "ppv_good", "ppv_disabled",
			"ppv_true", "ppv_wrongcol", "ppv_insertonly", "ppv_nopolicy", "ppv_offonly",
			"ppv_checktrue", "ppv_secondperm", "ppv_pipe", "ppv_restrictonly"} {
			_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.`+tbl+` CASCADE`)
		}
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.ppv_none    (id bigint primary key, tenant text not null);
CREATE TABLE atlantis.ppv_noforce (id bigint primary key, tenant text not null);
CREATE TABLE atlantis.ppv_good    (id bigint primary key, tenant text not null);
CREATE TABLE atlantis.ppv_disabled(id bigint primary key, tenant text not null);

ALTER TABLE atlantis.ppv_noforce ENABLE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_noforce USING (tenant = atlantis.current_partition());

ALTER TABLE atlantis.ppv_good ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_good FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_good USING (tenant = atlantis.current_partition());

CREATE POLICY p ON atlantis.ppv_disabled USING (tenant = atlantis.current_partition());

-- Three shapes that pass a policy COUNT and isolate nothing.
CREATE TABLE atlantis.ppv_true       (id bigint primary key, tenant text not null);
CREATE TABLE atlantis.ppv_wrongcol   (id bigint primary key, tenant text not null, other text);
CREATE TABLE atlantis.ppv_insertonly (id bigint primary key, tenant text not null);

ALTER TABLE atlantis.ppv_true ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_true FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_true USING (true);

ALTER TABLE atlantis.ppv_wrongcol ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_wrongcol FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_wrongcol USING (other = atlantis.current_partition());

ALTER TABLE atlantis.ppv_insertonly ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_insertonly FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_insertonly FOR INSERT
  WITH CHECK (tenant = atlantis.current_partition());

-- One condition each, so no branch can stand in for another. ppv_none trips
-- three at once and ppv_disabled two, which is why deleting a branch used to go
-- unnoticed.
CREATE TABLE atlantis.ppv_nopolicy (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.ppv_nopolicy ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_nopolicy FORCE ROW LEVEL SECURITY;

CREATE TABLE atlantis.ppv_offonly (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.ppv_offonly FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_offonly USING (tenant = atlantis.current_partition());

-- Reads scoped, writes not. A check reading only polqual calls this healthy,
-- and a request bound to one tenant INSERTs a row stamped with another.
CREATE TABLE atlantis.ppv_checktrue (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.ppv_checktrue ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_checktrue FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_checktrue
  USING (tenant = atlantis.current_partition()) WITH CHECK (true);

-- A CORRECT policy beside a permissive USING (true). Permissive policies are
-- OR'd, so the pair is USING (true); a check that accepted any matching policy
-- reported this table healthy while it returned every tenant's rows.
CREATE TABLE atlantis.ppv_secondperm (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.ppv_secondperm ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_secondperm FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_secondperm
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY leak_all ON atlantis.ppv_secondperm USING (true) WITH CHECK (true);

-- Correctly isolated, with a pipe character in the predicate. A FALSE
-- NEGATIVE probe: the previous aggregate split on the first pipe, so this table — the
-- only correctly isolated one in its group — was the only one reported broken,
-- refusing boot and every hot reload on a healthy deployment.
CREATE TABLE atlantis.ppv_pipe (id bigint primary key, tenant text not null, a text, b text);
ALTER TABLE atlantis.ppv_pipe ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_pipe FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_pipe
  USING (coalesce(a || b, '') <> 'x|y' AND tenant = atlantis.current_partition())
  WITH CHECK (coalesce(a || b, '') <> 'x|y' AND tenant = atlantis.current_partition());

-- RESTRICTIVE only. Restrictive policies AND with the permissive set, and with
-- no permissive policy PostgreSQL denies everything. Fail-closed, but it is
-- another shape that passes a count and isolates nothing.
CREATE TABLE atlantis.ppv_restrictonly (id bigint primary key, tenant text not null);
ALTER TABLE atlantis.ppv_restrictonly ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.ppv_restrictonly FORCE ROW LEVEL SECURITY;
CREATE POLICY p ON atlantis.ppv_restrictonly AS RESTRICTIVE
  USING (tenant = atlantis.current_partition());`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	tables := []PartitionedTable{
		{EntityID: "t.None", Schema: "atlantis", Table: "ppv_none", Column: "tenant"},
		{EntityID: "t.NoForce", Schema: "atlantis", Table: "ppv_noforce", Column: "tenant"},
		{EntityID: "t.Good", Schema: "atlantis", Table: "ppv_good", Column: "tenant"},
		{EntityID: "t.Disabled", Schema: "atlantis", Table: "ppv_disabled", Column: "tenant"},
		// A policy can exist, be enabled and be forced while filtering nothing.
		// Counting rows in pg_policy says yes to all three of these.
		{EntityID: "t.True", Schema: "atlantis", Table: "ppv_true", Column: "tenant"},
		{EntityID: "t.WrongCol", Schema: "atlantis", Table: "ppv_wrongcol", Column: "tenant"},
		{EntityID: "t.InsertOnly", Schema: "atlantis", Table: "ppv_insertonly", Column: "tenant"},
		// Isolated: enabled and forced, simply no policy.
		{EntityID: "t.NoPolicy", Schema: "atlantis", Table: "ppv_nopolicy", Column: "tenant"},
		// Isolated: a correct policy, forced, with RLS switched off.
		{EntityID: "t.OffOnly", Schema: "atlantis", Table: "ppv_offonly", Column: "tenant"},
		// A table that is named in the checkpoint and does not exist IS a
		// problem: only ApplyMigration writes the checkpoint, so the table
		// should be there. Treating absence as fine turned a wrong-name bug
		// into a clean report.
		{EntityID: "t.Absent", Schema: "atlantis", Table: "ppv_absent", Column: "tenant"},
		// Reads scoped, writes wide open.
		{EntityID: "t.CheckTrue", Schema: "atlantis", Table: "ppv_checktrue", Column: "tenant"},
		// A correct policy is present; a second permissive one defeats it.
		{EntityID: "t.SecondPerm", Schema: "atlantis", Table: "ppv_secondperm", Column: "tenant"},
		// Correctly isolated. Reporting this is a false NEGATIVE that refuses a
		// healthy deployment, which is the more expensive direction here.
		{EntityID: "t.Pipe", Schema: "atlantis", Table: "ppv_pipe", Column: "tenant"},
		{EntityID: "t.RestrictOnly", Schema: "atlantis", Table: "ppv_restrictonly", Column: "tenant"},
	}

	problems, err := VerifyPartitionPolicies(ctx, pool, tables)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	joined := strings.Join(problems, "\n")

	// Each entity must be reported for ITS OWN reason.
	//
	// Asserting only that the name appears let two of the four diagnostics be
	// deleted with the test green: ppv_none trips three conditions at once and
	// ppv_disabled trips two, so any surviving branch covered for the others.
	// The "carries no row-level security policy" message — the exact case this
	// check was written for — was the least pinned of the four.
	for _, want := range []struct{ entity, reason string }{
		{"t.None", "carries no row-level security policy"},
		{"t.NoForce", "without FORCE"},
		{"t.Disabled", "row-level security is disabled"},
		{"t.True", "does not compare that column"},
		{"t.WrongCol", "does not compare that column"},
		// Reported for the sharper reason now: a FOR INSERT policy is not a
		// weak read policy, it is the absence of one.
		{"t.InsertOnly", "none of them permissive"},
		{"t.Absent", "no table"},
		{"t.NoPolicy", "carries no row-level security policy"},
		{"t.OffOnly", "row-level security is disabled"},
		{"t.CheckTrue", "WITH CHECK does not compare"},
		{"t.SecondPerm", "permissive read policy"},
		{"t.RestrictOnly", "none of them permissive"},
	} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want.entity) && strings.Contains(p, want.reason) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not reported with the reason %q. Its table cannot "+
				"enforce the policy, so every read returns every tenant's rows:\n%s",
				want.entity, want.reason, joined)
		}
	}
	// A correctly isolated table must NOT be reported. t.Pipe is here because a
	// false negative on this check refuses boot AND every hot reload on a healthy
	// deployment — strictly worse than the false positive it was traded for.
	for _, notWant := range []string{"t.Good", "t.Pipe"} {
		if strings.Contains(joined, notWant) {
			t.Errorf("%s was reported and should not be:\n%s", notWant, joined)
		}
	}

	// And the refusal must be gated on the flag, not on the findings alone.
	if err := PartitionPolicyError(problems, false); err != nil {
		t.Errorf("refused to start with the flag off: %v", err)
	}
	if err := PartitionPolicyError(problems, true); err == nil {
		t.Errorf("started with the flag on and %d partitioned tables that enforce "+
			"nothing", len(problems))
	}
	if err := PartitionPolicyError(nil, true); err != nil {
		t.Errorf("refused to start with no findings: %v", err)
	}
}

// A policy on the wrong column must not be accepted because the declared name
// happens to appear inside the predicate text.
//
// A substring test reported `(other = atlantis.current_partition())` as scoping
// the columns `t`, `at` and `partition` — the last because the name occurs
// inside the function's own qualified name.
func TestPredicateScopesColumnMatchesWholeIdentifiers(t *testing.T) {
	const right = `(tenant = atlantis.current_partition())`
	const wrong = `(other = atlantis.current_partition())`
	const prefixed = `(tenant_backup = atlantis.current_partition())`

	for _, tc := range []struct {
		qual, column string
		want         bool
	}{
		{right, "tenant", true},
		{`("tenant" = atlantis.current_partition())`, "tenant", true},
		{wrong, "other", true},

		{wrong, "tenant", false},
		{wrong, "t", false},
		{wrong, "at", false},
		{wrong, "partition", false},
		{prefixed, "tenant", false},
		// No discriminator function at all.
		{`(true)`, "tenant", false},
		{``, "tenant", false},

		// A `|` anywhere in the predicate. The aggregate this replaced split on
		// the first one and returned false for every shape below, reporting a
		// correctly isolated table as broken.
		{`((tenant = atlantis.current_partition()) AND ((a || b) <> ''))`, "tenant", true},
		{`((tenant = atlantis.current_partition()) AND (id::text <> 'a|b'))`, "tenant", true},
		{`((tenant = atlantis.current_partition()) AND (body ~ 'x|y'))`, "tenant", true},
		{`((tenant = atlantis.current_partition()) AND ((flags | 4) > 0))`, "tenant", true},
		// The pipe BEFORE the comparison, which is the ordering that catches a
		// parse truncating at the first one. Every case above survives that
		// mutation by accident: their comparison sits in the retained prefix.
		{`((coalesce(a || b, '') <> 'x') AND (tenant = atlantis.current_partition()))`, "tenant", true},
		{`((body ~ 'x|y') AND (tenant = atlantis.current_partition()))`, "tenant", true},
	} {
		if got := predicateScopesColumn(tc.qual, tc.column); got != tc.want {
			t.Errorf("predicateScopesColumn(%q, %q) = %v, want %v. A false positive "+
				"reports a table as isolated when its policy filters on some other "+
				"column; a false negative refuses to start a healthy deployment",
				tc.qual, tc.column, got, tc.want)
		}
	}
}

// polcmd decides which half of a policy is asked about, and getting it wrong in
// either direction hides a real gap: a FOR INSERT policy is not a read policy,
// and a FOR DELETE policy has no WITH CHECK to be missing.
func TestPolicyCommandClassification(t *testing.T) {
	for _, tc := range []struct {
		cmd               string
		read, write, name string
	}{
		{"*", "yes", "yes", "ALL"},
		{"r", "yes", "no", "SELECT"},
		{"a", "no", "yes", "INSERT"},
		{"w", "yes", "yes", "UPDATE"},
		{"d", "yes", "no", "DELETE"},
	} {
		if got := yesNo(governsRead(tc.cmd)); got != tc.read {
			t.Errorf("governsRead(%q) = %s for %s, want %s", tc.cmd, got, tc.name, tc.read)
		}
		if got := yesNo(governsWrite(tc.cmd)); got != tc.write {
			t.Errorf("governsWrite(%q) = %s for %s, want %s", tc.cmd, got, tc.name, tc.write)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
