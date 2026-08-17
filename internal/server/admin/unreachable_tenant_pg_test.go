package admin

import (
	"context"
	"strings"
	"testing"
)

// Turning on `partition by` must not silently strand rows nobody can read.
//
// # The failure
//
// `''` is legal in a NOT NULL text column and is what a legacy discriminator
// carries after an ADD COLUMN with a default. Migration 0024 refuses to bind an
// empty tenant — set_partition('') errors, and current_partition() NULLIFs ''
// — which is what makes an unbound request fail closed. The consequence at the
// moment isolation is switched on over that data is that those rows become
// readable by nobody, with nothing reporting it, because the policy is doing
// exactly what it says.
//
// # The three cases, and why all three
//
// Refusing when rows are stranded is only half a guard. A check that refused
// every partition_added would also pass that case, and would make the feature
// unusable — so the clean table is driven too. And the override has to work,
// or an operator who has decided those rows are expendable has no way through
// except editing data they meant to abandon.

const unreachV1 = `
entity Doc in unre {
  id     bigint primary
  tenant varchar(16) not null
  body   text
}
`

const unreachV2 = `
entity Doc in unre {
  id     bigint primary
  tenant varchar(16) not null
  body   text
  partition by tenant
}
`

// applyPartitionAfterApproval runs the real sequence and returns the error from
// the apply that actually reaches the unreachable-tenant check.
//
// Adding `partition by` is cross-caller-breaking, and 0026 seeds that class
// require_approval=true, so the FIRST apply is always refused by the change
// policy and records a plan. Without clearing that gate these tests would
// assert against the approval refusal — which is non-nil, so the refusal case
// would have passed for entirely the wrong reason.
func applyPartitionAfterApproval(t *testing.T, svc *Service, src string) error {
	t.Helper()
	plan := depScopePlan(t, svc, "unre", "doc.atl", src)
	if err := depScopeApply(t, svc, "unre", "doc.atl", src, plan); err == nil {
		t.Fatal("adding `partition by` applied with no approval, so the change " +
			"policy is not gating it and the sequence below is not the real one")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())
	return depScopeApply(t, svc, "unre", "doc.atl", src, plan)
}

// seedUnreachable puts the table in place with the given tenant values, before
// `partition by` is added.
func seedUnreachable(t *testing.T, svc *Service, tenants ...string) {
	t.Helper()
	depScopePlanAndApply(t, svc, "unre", "doc.atl", unreachV1)
	for i, tenant := range tenants {
		if _, err := svc.pool.Exec(context.Background(),
			`INSERT INTO atlantis.unre_doc (id, tenant, body) VALUES ($1, $2, 'x')`,
			i+1, tenant); err != nil {
			t.Fatalf("seed %q: %v", tenant, err)
		}
	}
}

func TestApplyRefusesToStrandRowsWithAnEmptyTenant(t *testing.T) {
	svc := depScopeService(t)
	// One real tenant and two legacy rows, so the refusal has to be about the
	// empty ones rather than about the table having rows at all.
	seedUnreachable(t, svc, "acme", "", "")

	err := applyPartitionAfterApproval(t, svc, unreachV2)
	if err == nil {
		t.Fatal("the apply turned on tenant isolation over rows whose discriminator " +
			"is '', which no caller can bind to. Those rows are now readable by " +
			"nobody and nothing anywhere says so")
	}
	for _, want := range []string{"unreachable", "unre_doc", "tenant", "ATLANTIS_ALLOW_UNREACHABLE_TENANT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not tell the "+
				"operator what is wrong or what to do: %v", want, err)
		}
	}
	// The count has to be real, not a placeholder — an operator decides whether
	// to backfill or delete based on how many rows this is.
	if !strings.Contains(err.Error(), "2 rows") {
		t.Errorf("the refusal does not report how many rows are affected: %v", err)
	}

	// And the DDL must not have run.
	var n int
	if qerr := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid
 WHERE c.relname = 'unre_doc'`).Scan(&n); qerr != nil {
		t.Fatalf("count policies: %v", qerr)
	}
	if n != 0 {
		t.Errorf("the refused apply created %d policies anyway, so the refusal is a "+
			"message rather than a gate", n)
	}
}

// The other half: a table with no empty discriminators applies normally.
//
// Without this, a check that refused every partition_added would pass the test
// above and make the feature unusable.
func TestApplyAllowsTenantIsolationWhenEveryRowIsBindable(t *testing.T) {
	svc := depScopeService(t)
	seedUnreachable(t, svc, "acme", "globex")

	if err := applyPartitionAfterApproval(t, svc, unreachV2); err != nil {
		t.Fatalf("the apply was refused on a table where every row carries a real "+
			"tenant, which is the ordinary case this feature exists for: %v", err)
	}
}

// And the override lets an operator proceed knowingly.
func TestTheUnreachableTenantCheckCanBeOverridden(t *testing.T) {
	svc := depScopeService(t)
	seedUnreachable(t, svc, "acme", "")
	t.Setenv("ATLANTIS_ALLOW_UNREACHABLE_TENANT", "1")

	if err := applyPartitionAfterApproval(t, svc, unreachV2); err != nil {
		t.Fatalf("ATLANTIS_ALLOW_UNREACHABLE_TENANT=1 did not let the apply through, "+
			"so an operator who has decided those rows are expendable has no way "+
			"past this except editing data they meant to abandon: %v", err)
	}
}
