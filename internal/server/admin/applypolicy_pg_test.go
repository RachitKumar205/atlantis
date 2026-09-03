package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The apply-policy tier, driven through the two RPCs that run schema SQL.

const (
	tierV1 = `
entity Coil in %s {
  id   bigint primary
  note text
}
`
	tierV2Additive = `
entity Coil in %s {
  id    bigint primary
  note  text
  extra text
}
`
	tierV2Destructive = `
entity Coil in %s {
  id bigint primary
}
`
	tierV2Backfill = `
entity Coil in %s {
  id    bigint primary
  note  text
  label text not null backfill "'unknown'"
}
`
)

func tierFixture(t *testing.T, svc *Service, ns string) {
	t.Helper()
	depScopePlanAndApply(t, svc, ns, "coil.atl", tierSrc(tierV1, ns))
	if _, err := svc.RegisterCaller(context.Background(), &adminpb.RegisterCallerRequest{
		Caller: ns, CanMutate: true, CreatedBy: "tier-test",
	}); err != nil {
		t.Fatalf("RegisterCaller(%s): %v", ns, err)
	}
}

func tierSrc(tpl, ns string) string { return strings.ReplaceAll(tpl, "%s", ns) }

func setTier(t *testing.T, svc *Service, caller, tier string) {
	t.Helper()
	if _, err := svc.SetApplyPolicy(context.Background(), &adminpb.SetApplyPolicyRequest{
		Caller: caller, ApplyPolicy: tier, UpdatedBy: "tier-test",
	}); err != nil {
		t.Fatalf("SetApplyPolicy(%s, %s): %v", caller, tier, err)
	}
}

func TestSandboxOnlyRefusesApplyOutright(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tiersbx")
	setTier(t, svc, "tiersbx", "sandbox_only")

	plan := depScopePlan(t, svc, "tiersbx", "coil.atl", tierSrc(tierV2Additive, "tiersbx"))
	err := depScopeApply(t, svc, "tiersbx", "coil.atl", tierSrc(tierV2Additive, "tiersbx"), plan)
	if err == nil {
		t.Fatal("a sandbox_only caller applied an additive change")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "sandbox_only") {
		t.Errorf("the refusal does not name the tier: %v", err)
	}

	// Refused, not queued: no approval can make this caller one that applies,
	// so filing a plan would put an undecidable row in front of a reviewer.
	var n int
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM atlantis.schema_plans WHERE plan_id = $1`,
		plan.GetPlanId()).Scan(&n); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if n != 0 {
		t.Errorf("%d plan rows filed for a sandbox_only refusal, want 0", n)
	}
}

func TestSandboxOnlyRefusesTheBackfillPathToo(t *testing.T) {
	base := depScopeService(t)
	svc := backfillService(t, base)
	tierFixture(t, svc, "tierbf")
	setTier(t, svc, "tierbf", "sandbox_only")

	plan := depScopePlan(t, svc, "tierbf", "coil.atl", tierSrc(tierV2Backfill, "tierbf"))
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED {
		t.Fatalf("the change planned as %s, not backfill-required", plan.GetClass())
	}
	_, err := svc.BeginBackfillPlan(context.Background(), &adminpb.BeginBackfillPlanRequest{
		Caller: "tierbf", PlanId: plan.GetPlanId(),
		Files: depScopeFiles("coil.atl", tierSrc(tierV2Backfill, "tierbf")),
	})
	if err == nil {
		t.Fatal("a sandbox_only caller ran pre-backfill DDL through BeginBackfillPlan")
	}
	if !strings.Contains(err.Error(), "sandbox_only") {
		t.Errorf("the refusal does not name the tier: %v", err)
	}
}

func TestAlwaysAskQueuesAnAdditiveChange(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tierask")
	setTier(t, svc, "tierask", "always_ask")

	plan := depScopePlan(t, svc, "tierask", "coil.atl", tierSrc(tierV2Additive, "tierask"))
	err := depScopeApply(t, svc, "tierask", "coil.atl", tierSrc(tierV2Additive, "tierask"), plan)
	if err == nil {
		t.Fatal("an always_ask caller applied unattended")
	}
	// Queued by the tier, and the refusal says so — the change policy has no
	// rule for additive, so without the annotation the operator reads a
	// refusal the settings page cannot explain.
	if !strings.Contains(err.Error(), "apply policy (always_ask)") {
		t.Errorf("the refusal does not name the tier that queued it: %v", err)
	}

	// An approval unblocks it, exactly as a change-policy refusal would.
	approveStoredPlan(t, svc, plan.GetPlanId())
	if err := depScopeApply(t, svc, "tierask", "coil.atl", tierSrc(tierV2Additive, "tierask"), plan); err != nil {
		t.Fatalf("the approved change was still refused: %v", err)
	}

	// A human authorized it, so no tier is stamped on the version row.
	var policy string
	if err := svc.pool.QueryRow(context.Background(), `
SELECT applied_under_policy FROM atlantis.schema_versions
 WHERE caller = 'tierask' ORDER BY version DESC LIMIT 1`).Scan(&policy); err != nil {
		t.Fatalf("read the version row: %v", err)
	}
	if policy != "" {
		t.Errorf("applied_under_policy = %q for a human-approved apply, want empty", policy)
	}
}

func TestAnUnattendedApplyRecordsTheTierThatAuthorizedIt(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tierstamp")

	// auto_safe is the default; the additive change applies unattended.
	depScopePlanAndApply(t, svc, "tierstamp", "coil.atl", tierSrc(tierV2Additive, "tierstamp"))

	var policy, verdict string
	if err := svc.pool.QueryRow(context.Background(), `
SELECT applied_under_policy, applied_verdict FROM atlantis.schema_versions
 WHERE caller = 'tierstamp' ORDER BY version DESC LIMIT 1`).Scan(&policy, &verdict); err != nil {
		t.Fatalf("read the version row: %v", err)
	}
	if policy != "auto_safe" {
		t.Errorf("applied_under_policy = %q, want auto_safe", policy)
	}
	if verdict != "" {
		t.Errorf("applied_verdict = %q with no rehearsal consumed, want empty", verdict)
	}
}

// unverified never auto-applies: a destructive change under auto_all, with
// the change policy relaxed, still queues because no rehearsal verdict
// exists. This is the row the whole verdict model hangs on.
func TestAutoAllStillQueuesAnUnverifiedDestructiveChange(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tierauto")
	setTier(t, svc, "tierauto", "auto_all")

	if _, err := svc.SetChangePolicy(context.Background(), &adminpb.SetChangePolicyRequest{
		UpdatedBy: "tier-test",
		Entries: []*adminpb.ChangePolicyEntry{{
			ChangeClass:     adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
			RequireApproval: false,
		}},
	}); err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}

	plan := depScopePlan(t, svc, "tierauto", "coil.atl", tierSrc(tierV2Destructive, "tierauto"))
	err := depScopeApply(t, svc, "tierauto", "coil.atl", tierSrc(tierV2Destructive, "tierauto"), plan)
	if err == nil {
		t.Fatal("an unverified destructive change auto-applied at auto_all")
	}
	if !strings.Contains(err.Error(), "apply policy (auto_all)") {
		t.Errorf("the refusal does not name the tier: %v", err)
	}
}

func TestSetApplyPolicyValidatesAndRecords(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tierset")
	ctx := context.Background()

	if _, err := svc.SetApplyPolicy(ctx, &adminpb.SetApplyPolicyRequest{
		Caller: "tierset", ApplyPolicy: "auto-safe",
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a near-miss tier got %v, want InvalidArgument — stored, it would "+
			"silently demote the caller to always_ask", err)
	}
	if _, err := svc.SetApplyPolicy(ctx, &adminpb.SetApplyPolicyRequest{
		Caller: "nobody-here", ApplyPolicy: "auto_all",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("an unknown caller got %v, want NotFound", err)
	}

	if _, err := svc.SetApplyPolicy(ctx, &adminpb.SetApplyPolicyRequest{
		Caller: "tierset", ApplyPolicy: "always_ask",
		UpdatedBy: "console:abc", UpdatedByEmail: "op@example.com",
	}); err != nil {
		t.Fatalf("SetApplyPolicy: %v", err)
	}

	var kind, actor string
	var payload []byte
	if err := svc.pool.QueryRow(ctx, `
SELECT kind, actor, payload FROM atlantis.policy_events ORDER BY id DESC LIMIT 1`).
		Scan(&kind, &actor, &payload); err != nil {
		t.Fatalf("read policy_events: %v", err)
	}
	if kind != "apply_policy_set" || actor != "console:abc" {
		t.Errorf("event = (%q, %q), want (apply_policy_set, console:abc)", kind, actor)
	}
	var recorded map[string]any
	if err := json.Unmarshal(payload, &recorded); err != nil {
		t.Fatalf("the event payload is not an object: %v", err)
	}
	if recorded["set"] != "always_ask" || recorded["caller"] != "tierset" {
		t.Errorf("the event does not carry what was set: %s", payload)
	}

	got, err := svc.GetApplyPolicy(ctx, &adminpb.GetApplyPolicyRequest{Caller: "tierset"})
	if err != nil {
		t.Fatalf("GetApplyPolicy: %v", err)
	}
	if got.GetApplyPolicy() != "always_ask" || got.GetEffectiveApplyPolicy() != "always_ask" {
		t.Errorf("read back (%q, %q), want always_ask twice",
			got.GetApplyPolicy(), got.GetEffectiveApplyPolicy())
	}
}

// Re-registration reconciles capabilities; the tier is not a capability and
// must survive it, or a routine re-register silently re-arms a demoted caller.
func TestReRegistrationPreservesTheTier(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tierkeep")
	setTier(t, svc, "tierkeep", "sandbox_only")

	if _, err := svc.RegisterCaller(context.Background(), &adminpb.RegisterCallerRequest{
		Caller: "tierkeep", CanMutate: true, CreatedBy: "tier-test",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, err := svc.GetApplyPolicy(context.Background(), &adminpb.GetApplyPolicyRequest{Caller: "tierkeep"})
	if err != nil {
		t.Fatalf("GetApplyPolicy: %v", err)
	}
	if got.GetApplyPolicy() != "sandbox_only" {
		t.Errorf("apply_policy after re-registration = %q, want sandbox_only", got.GetApplyPolicy())
	}
}

// A stored value this build does not know queues everything. Written around
// the RPC's own validation, because that is how such a row arrives: a newer
// build wrote a tier this one has never heard of.
func TestAnUnknownStoredTierQueuesEverything(t *testing.T) {
	svc := depScopeService(t)
	tierFixture(t, svc, "tieryolo")
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE atlantis.caller_identities SET apply_policy = 'yolo' WHERE caller = 'tieryolo'`); err != nil {
		t.Fatalf("write the unknown tier: %v", err)
	}

	plan := depScopePlan(t, svc, "tieryolo", "coil.atl", tierSrc(tierV2Additive, "tieryolo"))
	err := depScopeApply(t, svc, "tieryolo", "coil.atl", tierSrc(tierV2Additive, "tieryolo"), plan)
	if err == nil {
		t.Fatal("an additive change applied under a tier this build does not recognise")
	}
	if !strings.Contains(err.Error(), "always_ask") {
		t.Errorf("the refusal does not say the caller is being treated as always_ask: %v", err)
	}
}
