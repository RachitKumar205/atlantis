package admin

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The decision RPCs, driven through the handlers against a live database.
//
// destructive_gate_test.go approves by writing the row directly, which keeps
// those tests about the gate. These are about the decision: who may make one,
// on which plans, and what a caller is told afterwards.

const (
	planLedgerV1 = `
entity Book in dsplan {
  id   bigint primary
  note text
}
`
	planLedgerV2 = `
entity Book in dsplan {
  id bigint primary
}
`
)

// pendingPlan drives an apply far enough to leave a request waiting, and
// returns its id. The refusal is the expected outcome, not a failure.
func pendingPlan(t *testing.T, svc *Service) string {
	t.Helper()
	depScopePlanAndApply(t, svc, "dsplan", "book.atl", planLedgerV1)
	plan := depScopePlan(t, svc, "dsplan", "book.atl", planLedgerV2)
	if err := depScopeApply(t, svc, "dsplan", "book.atl", planLedgerV2, plan); err == nil {
		t.Fatal("the destructive apply was not refused, so no plan is waiting")
	}
	return plan.GetPlanId()
}

// The queue has to show what is waiting, with enough on each row for somebody
// to decide whether to open it.
func TestListSchemaPlansShowsTheQueue(t *testing.T) {
	svc := depScopeService(t)
	planID := pendingPlan(t, svc)
	ctx := context.Background()

	resp, err := svc.ListSchemaPlans(ctx, &adminpb.ListSchemaPlansRequest{State: planPending})
	if err != nil {
		t.Fatalf("ListSchemaPlans: %v", err)
	}
	if len(resp.GetPlans()) != 1 {
		t.Fatalf("queue holds %d plans, want 1", len(resp.GetPlans()))
	}
	p := resp.GetPlans()[0]
	if p.GetPlanId() != planID {
		t.Errorf("plan_id = %q, want %q", p.GetPlanId(), planID)
	}
	if p.GetChangeClass() != adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE {
		t.Errorf("change_class = %s, want DESTRUCTIVE", p.GetChangeClass())
	}
	if p.GetRequestedBy() != "dsplan" {
		t.Errorf("requested_by = %q, want the caller that asked", p.GetRequestedBy())
	}
	// The role comes from the policy as it stands now, so the console does not
	// have to join the two itself and cannot show a stale one.
	if p.GetApproverRole() != DefaultApproverRole {
		t.Errorf("approver_role = %q, want %q", p.GetApproverRole(), DefaultApproverRole)
	}
	if p.GetExpired() {
		t.Error("a plan filed moments ago reports as expired")
	}
	if p.GetExpiresAt() == "" {
		t.Error("no expiry reported, so the console cannot show how long is left")
	}

	// Filtering by a state nothing is in must return nothing rather than
	// everything — a filter that silently degrades to "all" would show an
	// operator applied plans in the approval queue.
	empty, err := svc.ListSchemaPlans(ctx, &adminpb.ListSchemaPlansRequest{State: planApplied})
	if err != nil {
		t.Fatalf("ListSchemaPlans(applied): %v", err)
	}
	if len(empty.GetPlans()) != 0 {
		t.Errorf("filtering by %q returned %d plans", planApplied, len(empty.GetPlans()))
	}
}

// A reviewer decides on schema, so the source has to be there — and readable.
func TestGetSchemaPlanReturnsTheProposedSource(t *testing.T) {
	svc := depScopeService(t)
	planID := pendingPlan(t, svc)

	resp, err := svc.GetSchemaPlan(context.Background(), &adminpb.GetSchemaPlanRequest{PlanId: planID})
	if err != nil {
		t.Fatalf("GetSchemaPlan: %v", err)
	}
	files := resp.GetPlan().GetFiles()
	if len(files) != 1 {
		t.Fatalf("plan carries %d files, want 1", len(files))
	}
	if files[0].GetPath() != "book.atl" {
		t.Errorf("path = %q", files[0].GetPath())
	}
	// Stored as text rather than base64, so the column is legible in psql and
	// the console does not have to decode it to show a diff.
	if got := string(files[0].GetContent()); !strings.Contains(got, "entity Book in dsplan") {
		t.Errorf("the stored source is not the .atl that was submitted: %q", got)
	}
	if !strings.Contains(resp.GetPlan().GetUpSql(), "DROP COLUMN") &&
		!strings.Contains(resp.GetPlan().GetUpSql(), "note") {
		t.Errorf("the emitted SQL does not describe the change: %q", resp.GetPlan().GetUpSql())
	}
	if resp.GetPlan().GetSummary().GetPlanId() != planID {
		t.Error("the detail does not carry its own summary")
	}

	_, err = svc.GetSchemaPlan(context.Background(), &adminpb.GetSchemaPlanRequest{PlanId: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("an unknown plan id returned %s, want NotFound", status.Code(err))
	}
}

// Approving must let the change through, and must be recorded.
func TestApproveSchemaPlanLetsTheApplyProceed(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	depScopePlanAndApply(t, svc, "dsplan", "book.atl", planLedgerV1)
	plan := depScopePlan(t, svc, "dsplan", "book.atl", planLedgerV2)
	if err := depScopeApply(t, svc, "dsplan", "book.atl", planLedgerV2, plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}

	decided, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "reviewer@example.com",
		DecidedByRole: DefaultApproverRole, Reason: "agreed in the migration review",
	})
	if err != nil {
		t.Fatalf("ApproveSchemaPlan: %v", err)
	}
	if decided.GetPlan().GetState() != planApproved {
		t.Errorf("state after approval = %q", decided.GetPlan().GetState())
	}
	if decided.GetPlan().GetDecidedBy() != "reviewer@example.com" || decided.GetPlan().GetDecidedAt() == "" {
		t.Error("the decision was not attributed; an unattributed approval is not an audit record")
	}

	if err := depScopeApply(t, svc, "dsplan", "book.atl", planLedgerV2, plan); err != nil {
		t.Fatalf("the approved change was still refused: %v", err)
	}
}

// The role check the console's middleware is not allowed to be the only one.
func TestADecisionMustAssertTheRoleThePolicyNames(t *testing.T) {
	svc := depScopeService(t)
	planID := pendingPlan(t, svc)
	ctx := context.Background()

	_, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: planID, DecidedBy: "viewer@example.com", DecidedByRole: "viewer",
	})
	if err == nil {
		t.Fatal("an approval asserting a role the policy does not name was accepted. " +
			"The console's middleware would then be the only thing standing between " +
			"a viewer and production DDL.")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %s, want PermissionDenied", got)
	}

	// Rejecting takes the same check. Without it, anyone who can reach the RPC
	// can block every deploy in the deployment.
	_, err = svc.RejectSchemaPlan(ctx, &adminpb.RejectSchemaPlanRequest{
		PlanId: planID, DecidedBy: "viewer@example.com", DecidedByRole: "viewer",
		Reason: "no",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("rejecting with the wrong role gave %s, want PermissionDenied", status.Code(err))
	}

	// And the plan must still be waiting rather than half-decided.
	if got := storedPlanState(t, svc, planID); got != planPending {
		t.Errorf("state after two refused decisions = %q, want %q", got, planPending)
	}
}

// The rejection reason is the whole of what the caller is told, so it is
// required and it has to reach them.
func TestRejectSchemaPlanCarriesItsReasonToTheCaller(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()
	depScopePlanAndApply(t, svc, "dsplan", "book.atl", planLedgerV1)
	plan := depScopePlan(t, svc, "dsplan", "book.atl", planLedgerV2)
	if err := depScopeApply(t, svc, "dsplan", "book.atl", planLedgerV2, plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}

	_, err := svc.RejectSchemaPlan(ctx, &adminpb.RejectSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "reviewer@example.com",
		DecidedByRole: DefaultApproverRole,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a rejection with no reason gave %s, want InvalidArgument — the caller "+
			"would be told no and nothing else", status.Code(err))
	}

	if _, err := svc.RejectSchemaPlan(ctx, &adminpb.RejectSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "reviewer@example.com",
		DecidedByRole: DefaultApproverRole, Reason: "the note column is still read by billing",
	}); err != nil {
		t.Fatalf("RejectSchemaPlan: %v", err)
	}

	err = depScopeApply(t, svc, "dsplan", "book.atl", planLedgerV2, plan)
	if err == nil {
		t.Fatal("a rejected plan applied")
	}
	if !strings.Contains(err.Error(), "the note column is still read by billing") {
		t.Errorf("the reason did not reach the caller, who now has to go and ask: %v", err)
	}
}

// A decision is for a plan awaiting one. Deciding twice, or deciding something
// already applied, must be refused rather than quietly overwriting the record
// of what happened.
func TestADecidedPlanCannotBeDecidedAgain(t *testing.T) {
	svc := depScopeService(t)
	planID := pendingPlan(t, svc)
	ctx := context.Background()

	approve := &adminpb.ApproveSchemaPlanRequest{
		PlanId: planID, DecidedBy: "first@example.com", DecidedByRole: DefaultApproverRole,
	}
	if _, err := svc.ApproveSchemaPlan(ctx, approve); err != nil {
		t.Fatalf("first approval: %v", err)
	}

	_, err := svc.RejectSchemaPlan(ctx, &adminpb.RejectSchemaPlanRequest{
		PlanId: planID, DecidedBy: "second@example.com",
		DecidedByRole: DefaultApproverRole, Reason: "changed my mind",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("rejecting an approved plan gave %s, want FailedPrecondition", status.Code(err))
	}

	// The first decision must be the one on record.
	resp, err := svc.GetSchemaPlan(ctx, &adminpb.GetSchemaPlanRequest{PlanId: planID})
	if err != nil {
		t.Fatalf("GetSchemaPlan: %v", err)
	}
	if got := resp.GetPlan().GetSummary().GetDecidedBy(); got != "first@example.com" {
		t.Errorf("decided_by = %q; a second decision overwrote the first", got)
	}
}
