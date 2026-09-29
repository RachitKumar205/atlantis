package admin

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Protected entities, freeze windows, override and the self-approval refusal,
// driven through the RPCs against a live database.

func putProtection(t *testing.T, svc *Service, pattern, floor string) {
	t.Helper()
	if _, err := svc.PutProtectedEntity(context.Background(), &adminpb.PutProtectedEntityRequest{
		Entity:    &adminpb.ProtectedEntity{Pattern: pattern, Floor: floor, Reason: "holds money"},
		UpdatedBy: "prot-test",
	}); err != nil {
		t.Fatalf("PutProtectedEntity(%s): %v", pattern, err)
	}
}

func applyWithActor(t *testing.T, svc *Service, caller, path, src, actor string, plan *adminpb.PlanSchemaResponse) error {
	t.Helper()
	_, err := svc.ApplyMigration(context.Background(), &adminpb.ApplyMigrationRequest{
		Caller:         caller,
		PlanId:         plan.GetPlanId(),
		Files:          depScopeFiles(path, src),
		CheckpointHash: plan.GetCheckpointHash(),
		Actor:          actor,
	})
	return err
}

func TestAProtectedEntityQueuesAnAdditiveChange(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "protadd")
	putProtection(t, svc, "protadd.*", "require_approval")

	// Additive under auto_safe would apply unattended; the protection is the
	// only thing in the way.
	plan := depScopePlan(t, svc, "protadd", "coil.atl", tierSrc(tierV2Additive, "protadd"))
	err := depScopeApply(t, svc, "protadd", "coil.atl", tierSrc(tierV2Additive, "protadd"), plan)
	if err == nil {
		t.Fatal("an additive change on a protected namespace applied unattended")
	}
	if !strings.Contains(err.Error(), "protadd.* is protected") {
		t.Errorf("the refusal does not name the protection: %v", err)
	}

	approveStoredPlan(t, svc, plan.GetPlanId())
	if err := depScopeApply(t, svc, "protadd", "coil.atl", tierSrc(tierV2Additive, "protadd"), plan); err != nil {
		t.Fatalf("the approved change was still refused: %v", err)
	}
}

func TestAnAdminOnlyFloorOverridesTheApproverRole(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "protfloor")
	ctx := context.Background()

	// The class rule names developer; the protection demands admin.
	if _, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "prot-test",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass:     adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
			RequireApproval: true,
			ApproverRole:    "developer",
		}},
	}); err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}
	putProtection(t, svc, "protfloor.Coil", "admin_only")

	plan := depScopePlan(t, svc, "protfloor", "coil.atl", tierSrc(tierV2Destructive, "protfloor"))
	if err := depScopeApply(t, svc, "protfloor", "coil.atl", tierSrc(tierV2Destructive, "protfloor"), plan); err == nil {
		t.Fatal("the gated apply was not refused")
	}

	_, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "dev@example.com", DecidedByRole: "developer",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a developer decided a plan under admin_only protection: %v", err)
	}
	if _, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "op@example.com", DecidedByRole: "admin",
	}); err != nil {
		t.Fatalf("an admin's decision was refused: %v", err)
	}
}

func TestAFreezeWindowHoldsEvenAnApprovedChange(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "frzhold")
	ctx := context.Background()

	now := time.Now().UTC()
	created, err := svc.CreateFreezeWindow(ctx, &adminpb.CreateFreezeWindowRequest{
		Window: &adminpb.FreezeWindow{
			StartsAt: now.Add(-time.Hour).Format(time.RFC3339),
			EndsAt:   now.Add(time.Hour).Format(time.RFC3339),
			Reason:   "launch weekend",
		},
		UpdatedBy: "frz-test",
	})
	if err != nil {
		t.Fatalf("CreateFreezeWindow: %v", err)
	}

	// Even an additive change under auto_safe waits.
	plan := depScopePlan(t, svc, "frzhold", "coil.atl", tierSrc(tierV2Additive, "frzhold"))
	err = depScopeApply(t, svc, "frzhold", "coil.atl", tierSrc(tierV2Additive, "frzhold"), plan)
	if err == nil {
		t.Fatal("an apply ran inside a freeze window")
	}
	if !strings.Contains(err.Error(), "freeze window") || !strings.Contains(err.Error(), "launch weekend") {
		t.Errorf("the refusal does not name the window and its reason: %v", err)
	}

	// Lifting the window lifts the hold.
	if _, err := svc.DeleteFreezeWindow(ctx, &adminpb.DeleteFreezeWindowRequest{
		Id: created.GetWindow().GetId(), UpdatedBy: "frz-test",
	}); err != nil {
		t.Fatalf("DeleteFreezeWindow: %v", err)
	}
	if err := depScopeApply(t, svc, "frzhold", "coil.atl", tierSrc(tierV2Additive, "frzhold"), plan); err != nil {
		t.Fatalf("the apply was still refused after the window lifted: %v", err)
	}
}

func TestAnOrdinaryApprovalStillWaitsOutTheFreeze(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "frzwait")
	ctx := context.Background()

	plan := depScopePlan(t, svc, "frzwait", "coil.atl", tierSrc(tierV2Destructive, "frzwait"))
	if err := depScopeApply(t, svc, "frzwait", "coil.atl", tierSrc(tierV2Destructive, "frzwait"), plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}
	now := time.Now().UTC()
	if _, err := svc.CreateFreezeWindow(ctx, &adminpb.CreateFreezeWindowRequest{
		Window: &adminpb.FreezeWindow{
			StartsAt: now.Add(-time.Hour).Format(time.RFC3339),
			EndsAt:   now.Add(time.Hour).Format(time.RFC3339),
		},
		UpdatedBy: "frz-test",
	}); err != nil {
		t.Fatalf("CreateFreezeWindow: %v", err)
	}

	// Approving during a freeze works — the decision is not the action —
	// and the approved change then waits behind the window.
	if _, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "op@example.com", DecidedByRole: "admin",
	}); err != nil {
		t.Fatalf("approving during a freeze was refused: %v", err)
	}
	err := depScopeApply(t, svc, "frzwait", "coil.atl", tierSrc(tierV2Destructive, "frzwait"), plan)
	if err == nil || !strings.Contains(err.Error(), "freeze window") {
		t.Fatalf("an ordinarily-approved change ran inside the freeze: %v", err)
	}
}

func TestAnOverrideApprovalPassesTheFreeze(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "frzover")
	ctx := context.Background()

	plan := depScopePlan(t, svc, "frzover", "coil.atl", tierSrc(tierV2Destructive, "frzover"))
	if err := depScopeApply(t, svc, "frzover", "coil.atl", tierSrc(tierV2Destructive, "frzover"), plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}
	now := time.Now().UTC()
	if _, err := svc.CreateFreezeWindow(ctx, &adminpb.CreateFreezeWindowRequest{
		Window: &adminpb.FreezeWindow{
			StartsAt: now.Add(-time.Hour).Format(time.RFC3339),
			EndsAt:   now.Add(time.Hour).Format(time.RFC3339),
		},
		UpdatedBy: "frz-test",
	}); err != nil {
		t.Fatalf("CreateFreezeWindow: %v", err)
	}

	if _, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "op@example.com", DecidedByRole: "admin",
		Override: true, Reason: "incident — the fix cannot wait for the window",
	}); err != nil {
		t.Fatalf("the override approval was refused: %v", err)
	}
	if err := depScopeApply(t, svc, "frzover", "coil.atl", tierSrc(tierV2Destructive, "frzover"), plan); err != nil {
		t.Fatalf("the overridden change was still held by the freeze: %v", err)
	}
}

func TestAnOverrideNeedsAReasonAndTheAdminRole(t *testing.T) {
	svc := depScopeService(t)
	planID := pendingPlan(t, svc)
	ctx := context.Background()

	_, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: planID, DecidedBy: "op@example.com", DecidedByRole: "admin", Override: true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an override with no reason got %v, want InvalidArgument", err)
	}
	_, err = svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: planID, DecidedBy: "dev@example.com", DecidedByRole: "developer",
		Override: true, Reason: "trust me",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a developer override got %v, want PermissionDenied", err)
	}
}

func TestARequestIsNotApprovedByThePersonWhoMadeIt(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "selfap")
	ctx := context.Background()

	// A destructive change, applied with a console actor, queues and records
	// who asked.
	plan := depScopePlan(t, svc, "selfap", "coil.atl", tierSrc(tierV2Destructive, "selfap"))
	if err := applyWithActor(t, svc, "selfap", "coil.atl", tierSrc(tierV2Destructive, "selfap"),
		"console:usr_1", plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}

	// The same person cannot approve it.
	_, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "one@example.com", DecidedByRole: "admin",
		DecidedByActor: "console:usr_1",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a self-approval got %v, want PermissionDenied", err)
	}
	if !strings.Contains(err.Error(), "console:usr_1") {
		t.Errorf("the refusal does not name the requester: %v", err)
	}

	// Rejecting one's own request is fine — the worst it costs is a re-plan.
	// (Proved by the absence of the check on the reject arm; asserted here so
	// symmetrising the two would go red.)
	// Another person approves it.
	summary, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "two@example.com", DecidedByRole: "admin",
		DecidedByActor: "console:usr_2",
	})
	if err != nil {
		t.Fatalf("a second person's approval was refused: %v", err)
	}
	if summary.GetPlan().GetRequestedByActor() != "console:usr_1" {
		t.Errorf("requested_by_actor = %q, want console:usr_1",
			summary.GetPlan().GetRequestedByActor())
	}
}

// TestAPlanRecordsWhoAskedByName reads the requester's name and email back
// through every RPC that returns a plan, and through the re-open of an
// expired request.
func TestAPlanRecordsWhoAskedByName(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "reqname")
	ctx := context.Background()

	src := tierSrc(tierV2Destructive, "reqname")
	plan := depScopePlan(t, svc, "reqname", "coil.atl", src)
	apply := func(actor, email, name string) {
		t.Helper()
		_, err := svc.ApplyMigration(ctx, &adminpb.ApplyMigrationRequest{
			Caller: "reqname", PlanId: plan.GetPlanId(), Files: depScopeFiles("coil.atl", src),
			CheckpointHash: plan.GetCheckpointHash(),
			Actor:          actor, ActorEmail: email, ActorName: name,
		})
		if err == nil {
			t.Fatal("the destructive apply was not refused")
		}
	}
	check := func(where string, got *adminpb.SchemaPlanSummary, actor, email, name string) {
		t.Helper()
		if got.GetRequestedByActor() != actor || got.GetRequestedByActorEmail() != email ||
			got.GetRequestedByActorName() != name {
			t.Errorf("%s: requester = %s %q <%s>, want %s %q <%s>", where,
				got.GetRequestedByActor(), got.GetRequestedByActorName(), got.GetRequestedByActorEmail(),
				actor, name, email)
		}
	}
	list := func() *adminpb.SchemaPlanSummary {
		t.Helper()
		out, err := svc.ListSchemaPlans(ctx, &adminpb.ListSchemaPlansRequest{Caller: "reqname"})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.GetPlans()) != 1 {
			t.Fatalf("ListSchemaPlans returned %d plans, want 1", len(out.GetPlans()))
		}
		return out.GetPlans()[0]
	}

	apply("console:usr_1", "ada@example.com", "Ada Lovelace")
	got, err := svc.GetSchemaPlan(ctx, &adminpb.GetSchemaPlanRequest{PlanId: plan.GetPlanId()})
	if err != nil {
		t.Fatal(err)
	}
	check("GetSchemaPlan", got.GetPlan().GetSummary(), "console:usr_1", "ada@example.com", "Ada Lovelace")

	// A pending request keeps the person who filed it, so the name shown
	// matches the principal the self-approval refusal compares.
	apply("console:usr_3", "grace@example.com", "Grace Hopper")
	check("ListSchemaPlans while pending", list(), "console:usr_1", "ada@example.com", "Ada Lovelace")

	// An expired request is re-filed by the next apply, under that apply's
	// requester.
	if _, err := svc.pool.Exec(ctx, `UPDATE atlantis.schema_plans
SET expires_at = now() - interval '1 minute' WHERE plan_id = $1`, plan.GetPlanId()); err != nil {
		t.Fatal(err)
	}
	apply("console:usr_3", "grace@example.com", "Grace Hopper")
	check("ListSchemaPlans after the re-open", list(), "console:usr_3", "grace@example.com", "Grace Hopper")

	approved, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "two@example.com", DecidedByRole: "admin",
		DecidedByActor: "console:usr_2",
	})
	if err != nil {
		t.Fatal(err)
	}
	check("ApproveSchemaPlan", approved.GetPlan(), "console:usr_3", "grace@example.com", "Grace Hopper")
}

func TestSelfApprovalYieldsToAnOverrideAndIsRecorded(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "selfov")
	ctx := context.Background()

	plan := depScopePlan(t, svc, "selfov", "coil.atl", tierSrc(tierV2Destructive, "selfov"))
	if err := applyWithActor(t, svc, "selfov", "coil.atl", tierSrc(tierV2Destructive, "selfov"),
		"console:usr_1", plan); err == nil {
		t.Fatal("the destructive apply was not refused")
	}

	summary, err := svc.ApproveSchemaPlan(ctx, &adminpb.ApproveSchemaPlanRequest{
		PlanId: plan.GetPlanId(), DecidedBy: "one@example.com", DecidedByRole: "admin",
		DecidedByActor: "console:usr_1", Override: true,
		Reason: "sole admin this week; reviewed against the plan text",
	})
	if err != nil {
		t.Fatalf("the self-approval override was refused: %v", err)
	}
	if got := summary.GetPlan().GetDecidedVia(); got != "self_approval_override" {
		t.Errorf("decided_via = %q, want self_approval_override", got)
	}

	var n int
	if err := svc.pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.policy_events WHERE kind = 'plan_override'`).Scan(&n); err != nil {
		t.Fatalf("count policy events: %v", err)
	}
	if n != 1 {
		t.Errorf("%d plan_override policy events, want 1", n)
	}
}
