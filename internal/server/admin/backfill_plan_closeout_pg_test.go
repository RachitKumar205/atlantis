package admin

import (
	"context"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// A gated backfill has to close its plan out, the way ApplyMigration does.
//
// BeginBackfillPlan passed the approval gate, ran the pre-backfill DDL,
// persisted the checkpoint and committed — and never called markPlanApplied.
// The row stayed `approved` with applied_version NULL forever. Two things
// break: the approval ledger has no record that the decision was acted on, so
// an auditor asking "was this applied, and as which version?" gets nothing from
// the row; and ListSchemaPlans(state='approved') keeps returning it, so the
// console's approved queue never drains.
//
// Backfill-required is gated by default — 0026 seeds it with
// require_approval=true — so this is the ordinary path, not a corner.

const (
	bfCloseoutV1 = `
entity Meter in bfclose {
  id bigint primary
}
`
	// Adding a NOT NULL column with no default is backfill-required, and the
	// `backfill` modifier is what gives BeginBackfillPlan an expression to run.
	bfCloseoutV2 = `
entity Meter in bfclose {
  id    bigint primary
  label text not null backfill "'unknown'"
}
`
)

func backfillService(t *testing.T, base *Service) *Service {
	t.Helper()
	return New(base.pool, Config{
		AllowApplyMutation: true,
		BackfillEnabled:    true,
	})
}

func TestAGatedBackfillClosesItsPlanOut(t *testing.T) {
	base := depScopeService(t)
	svc := backfillService(t, base)
	ctx := context.Background()

	depScopePlanAndApply(t, svc, "bfclose", "meter.atl", bfCloseoutV1)

	plan := depScopePlan(t, svc, "bfclose", "meter.atl", bfCloseoutV2)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED {
		t.Fatalf("the change planned as %s, not backfill-required — this test is "+
			"not exercising the path it names", plan.GetClass())
	}

	// Refused first, which is what files the plan row. Without this there is
	// nothing for the close-out to close.
	_, err := svc.BeginBackfillPlan(ctx, &adminpb.BeginBackfillPlanRequest{
		Caller: "bfclose", PlanId: plan.GetPlanId(),
		Files: depScopeFiles("meter.atl", bfCloseoutV2),
	})
	if err == nil {
		t.Fatal("a gated backfill ran with no approval")
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Fatalf("plan state = %q after the refusal, want %q", got, planPending)
	}

	approveStoredPlan(t, svc, plan.GetPlanId())

	if _, err := svc.BeginBackfillPlan(ctx, &adminpb.BeginBackfillPlanRequest{
		Caller: "bfclose", PlanId: plan.GetPlanId(),
		Files: depScopeFiles("meter.atl", bfCloseoutV2),
	}); err != nil {
		t.Fatalf("approved backfill was refused: %v", err)
	}

	// The point of the fix.
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planApplied {
		t.Errorf("plan state = %q after a successful backfill, want %q — the "+
			"approval ledger has no record the decision was acted on, and the "+
			"console's approved queue never drains", got, planApplied)
	}

	// applied_version is the other half: "was it applied" and "as which schema
	// version" are different questions, and an auditor needs both.
	var version *int64
	if err := svc.pool.QueryRow(ctx,
		`SELECT applied_version FROM atlantis.schema_plans WHERE plan_id = $1`,
		plan.GetPlanId()).Scan(&version); err != nil {
		t.Fatalf("read applied_version: %v", err)
	}
	if version == nil {
		t.Error("applied_version is NULL on a plan that was applied — the row " +
			"cannot say which schema version carried the change")
	}
}
