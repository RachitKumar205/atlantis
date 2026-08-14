package admin

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The apply gate, exercised through the handler against a live database.
//
// # This file used to read source instead
//
// It held two AST tests asserting that ApplyMigration mentioned
// codegen.ClassDestructive inside a condition that returns. That was the right
// guard for the shape it guarded: a flat refusal of two classes, written as an
// `if`, which a careless edit could drop without anything noticing.
//
// Those tests said so themselves — "this asserts the condition is present,
// never that it fires ... TestDirectApplyCannotSkipApproval, which calls the
// handler against a live database, is the one that proves it, and it arrives
// with the change-policy work". The change-policy work has arrived, and the
// refusal is no longer a class literal in an if: gateOnChangePolicy is
// deliberately class-agnostic and reads the rule from a table. Keeping the AST
// tests would have meant re-adding a class name to ApplyMigration so a test
// could find it — encoding the old fix rather than the property it protected.
//
// The property is unchanged and is what these assert: a direct gRPC apply
// cannot skip the human. Calling svc.ApplyMigration IS the direct-gRPC path —
// the capability interceptor only ever checked the capability, and it cannot
// see the request body, so it has no idea the change destroys anything.

const (
	gateLedgerV1 = `
entity Ledger in dsgate {
  id   bigint primary
  note text
}
`
	gateLedgerV2 = `
entity Ledger in dsgate {
  id bigint primary
}
`
)

// approveStoredPlan says yes the way the console will, before the decision RPCs
// exist. Writing the row directly is what keeps these tests about the gate
// rather than about the RPCs that will set the same column.
func approveStoredPlan(t *testing.T, svc *Service, planID string) {
	t.Helper()
	tag, err := svc.pool.Exec(context.Background(), `
UPDATE atlantis.schema_plans
   SET state = 'approved', decided_by = 'reviewer@example.com',
       decided_by_role = 'admin', decided_at = now()
 WHERE plan_id = $1 AND state = 'pending_approval'`, planID)
	if err != nil {
		t.Fatalf("approve %s: %v", planID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("approving %s updated %d rows, want 1 — the plan was not pending",
			planID, tag.RowsAffected())
	}
}

func storedPlanState(t *testing.T, svc *Service, planID string) string {
	t.Helper()
	var state string
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT state FROM atlantis.schema_plans WHERE plan_id = $1`, planID).Scan(&state); err != nil {
		t.Fatalf("read plan %s: %v", planID, err)
	}
	return state
}

// The hole the whole gate exists to close.
func TestDirectApplyCannotSkipApproval(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsgate", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsgate", "ledger.atl", gateLedgerV2)
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE {
		t.Fatalf("dropping a column planned as %s; this test is not exercising what "+
			"it thinks it is", plan.GetClass())
	}
	if !plan.GetRequiresApproval() {
		t.Fatal("the plan does not report requiring approval, so the gate below " +
			"would prove nothing about the policy")
	}

	err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("a destructive change applied through a direct handler call with no " +
			"human involved. The capability interceptor cannot see the request " +
			"body, so nothing else in the path knows this drops a column.")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition — clients branch on the "+
			"code rather than the message", got)
	}
	if !strings.Contains(err.Error(), "needs approval from admin") {
		t.Errorf("the refusal does not say who can approve, so the engineer it stops "+
			"has to go and ask: %v", err)
	}

	// And the attempt must have been recorded, or there is nothing for anyone to
	// approve and the refusal is a dead end.
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Errorf("plan state = %q, want %q", got, planPending)
	}

	// The column must still be there.
	var n int
	if err := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'dsgate_ledger' AND column_name = 'note'`).Scan(&n); err != nil {
		t.Fatalf("check the column: %v", err)
	}
	if n != 1 {
		t.Error("the refused apply dropped the column anyway")
	}
}

// The other half: an approval has to actually let the change through, or the
// gate is just a refusal with extra steps.
func TestApprovedPlanApplies(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsgate", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsgate", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("expected the first attempt to be refused and recorded")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())

	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan); err != nil {
		t.Fatalf("an approved plan was still refused: %v", err)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planApplied {
		t.Errorf("plan state after applying = %q, want %q", got, planApplied)
	}
}

// TestApprovalDoesNotTransferToDifferentFileContents is the one that matters
// most, and its name carries the reason.
//
// A reviewer approves a specific change. If the approval can be moved onto
// different content, the gate has been passed by something nobody read — and
// every other check in the apply path lets it through, because every other
// check is asking a different question.
func TestApprovalDoesNotTransferToDifferentFileContents(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsgate", "ledger.atl", gateLedgerV1)

	// Approved: drop the note column.
	approvedSrc := gateLedgerV2
	plan := depScopePlan(t, svc, "dsgate", "ledger.atl", approvedSrc)
	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", approvedSrc, plan); err == nil {
		t.Fatal("expected the first attempt to be refused and recorded")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())

	// Submitted: something else entirely, at the same path.
	const swapped = `
entity Ledger in dsgate {
  id      bigint primary
  balance numeric(10, 2) not null default 0
}
`
	err := depScopeApply(t, svc, "dsgate", "ledger.atl", swapped, plan)
	if err == nil {
		t.Fatal("an approval for one file set was honoured for a different one at " +
			"the same path. The reviewer approved dropping a column and something " +
			"else was applied under their decision.")
	}
	// It is refused by the plan id, which now covers contents — and that is the
	// point of hashing them. The approval never gets the chance to be consulted.
	if !strings.Contains(err.Error(), "stale") && !strings.Contains(err.Error(), "different file contents") {
		t.Errorf("refused, but not by a guard that knows the contents changed: %v", err)
	}
}

// Rejection is terminal. Without this, "rejected" means "rejected until the
// pipeline retries", and a pipeline retries every few minutes.
func TestRejectedPlanDoesNotReopen(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsgate", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsgate", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("expected the first attempt to be refused and recorded")
	}
	if _, err := svc.pool.Exec(context.Background(), `
UPDATE atlantis.schema_plans
   SET state = 'rejected', decided_by = 'reviewer@example.com',
       decision_reason = 'ask the data team first'
 WHERE plan_id = $1`, plan.GetPlanId()); err != nil {
		t.Fatalf("reject: %v", err)
	}

	err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("a rejected plan applied on resubmission")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("refused for some other reason than the rejection: %v", err)
	}
	// The reason has to reach the person who has to act on it.
	if !strings.Contains(err.Error(), "ask the data team first") {
		t.Errorf("the rejection reason is not surfaced, so the caller is told no "+
			"without being told why: %v", err)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planRejected {
		t.Errorf("resubmitting moved a rejected plan to %q", got)
	}
}

// A retrying pipeline must not undo a human's decision.
//
// This is the ON CONFLICT ... WHERE clause. Without it the upsert resets an
// approved plan to pending on every retry, so the operator approves, CI retries
// a minute later and un-approves it, and the two chase each other indefinitely
// while the plan never applies.
func TestARetryDoesNotResetAnApprovedPlan(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsgate", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsgate", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("expected the first attempt to be refused and recorded")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())

	// The apply that lands between the approval and the operator's next glance
	// at the console is the same apply, retried. It must succeed rather than
	// re-open the request.
	if err := depScopeApply(t, svc, "dsgate", "ledger.atl", gateLedgerV2, plan); err != nil {
		t.Fatalf("the retry after approval was refused: %v", err)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planApplied {
		t.Errorf("plan state = %q, want %q", got, planApplied)
	}
}
