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

// The apply gate, exercised through the handler against a live database.
//
// An AST test asserting that ApplyMigration mentions codegen.ClassDestructive
// inside a returning condition fits a flat refusal of two classes written as an
// `if`, which a careless edit can drop unnoticed. It does not fit
// gateOnChangePolicy, which is class-agnostic and reads the rule from a table:
// such a test would need a class name re-added to ApplyMigration so it could
// find one, encoding the fix rather than the property.
//
// The property is what these assert: a direct gRPC apply
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
	// Additive relative to V1, and therefore UNGATED: migration 0026 seeds
	// PLAN_CLASS_ADDITIVE with require_approval = false. That is what makes it
	// the right probe for the fail-closed path — a refusal of this change can
	// only have come from the policy read failing, never from a rule.
	gateLedgerV3Additive = `
entity Ledger in dsgate {
  id    bigint primary
  note  text
  extra text
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

// TestAnUngatedClassDoesNotCarryAGatedOneThroughWithIt is the exploit the gate
// used to allow.
//
// The change policy is FOUR independent rules, and the gate consulted only
// Diff.HighestClass(). Breaking outranks Destructive, so a diff that both
// renamed a table and dropped a column asked the breaking rule and never the
// destructive one. An operator who set breaking to auto-apply — a defensible
// setting when one team owns every caller — silently turned off the
// destructive gate they had explicitly left on. The DROP ran unattended and no
// plan row was filed, so there was nothing to review afterwards either.
//
// The fixture has to produce BOTH classes in one diff, and the assertion below
// checks that before it checks anything else. A diff carrying only one class
// would pass this test against the broken code.
func TestAnUngatedClassDoesNotCarryAGatedOneThroughWithIt(t *testing.T) {
	svc := depScopeService(t)
	ctx := context.Background()

	const v1 = `
entity Vault in mixgate {
  id   bigint primary
  note text
}
`
	// Renaming the physical table is Breaking; dropping `note` is Destructive.
	const v2 = `
entity Vault in mixgate {
  table "atlantis.mixgate_vault_renamed"
  id bigint primary
}
`
	depScopePlanAndApply(t, svc, "mixgate", "vault.atl", v1)

	// Breaking auto-applies; destructive still requires a human. A defensible
	// pairing when one team owns every caller — and the one that turned the
	// destructive gate off.
	if _, err := svc.SetChangePolicy(ctx, &adminpb.SetChangePolicyRequest{
		UpdatedBy: "operator",
		Entries: []*adminpb.ChangePolicyEntry{
			{
				ChangeClass:     adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING,
				RequireApproval: false,
			},
			{
				ChangeClass:     adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
				RequireApproval: true,
				ApproverRole:    "admin",
			},
		},
	}); err != nil {
		t.Fatalf("SetChangePolicy: %v", err)
	}

	plan := depScopePlan(t, svc, "mixgate", "vault.atl", v2)

	// The premise. Without both classes present this proves nothing, and the
	// broken gate would pass it.
	if plan.GetClass() != adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING {
		t.Fatalf("the diff planned as %s, so the ungated class is not the highest "+
			"one and this test is not exercising the defect", plan.GetClass())
	}
	// The destructive half, checked against the SQL that would actually run
	// rather than against a summary of it. A dropped column is parked, so the
	// migration renames it — if that is absent, the diff carries no destructive
	// change and the rest of this test proves nothing.
	if !strings.Contains(plan.GetUpSql(), "note__parked") {
		t.Fatalf("the migration parks no column, so the diff carries no "+
			"destructive change and this test is not exercising the defect:\n%s",
			plan.GetUpSql())
	}

	err := depScopeApply(t, svc, "mixgate", "vault.atl", v2, plan)
	if err == nil {
		t.Fatal("the apply went through. Breaking was set to auto-apply and the " +
			"diff ALSO drops a column, so consulting only the highest class " +
			"applied a destructive change the operator had gated — unattended, " +
			"with no plan recorded for anyone to review after the fact.")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	// Recorded under the rule that stopped it, not under the worst class.
	if !strings.Contains(err.Error(), "destructive") &&
		!strings.Contains(strings.ToLower(err.Error()), "destructive") {
		t.Errorf("the refusal does not name the destructive rule that caused it, "+
			"so the operator cannot tell which policy to change: %v", err)
	}

	// And the column survives.
	var n int
	if err := svc.pool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'mixgate_vault' AND column_name = 'note'`).Scan(&n); err != nil {
		t.Fatalf("check the column: %v", err)
	}
	if n != 1 {
		t.Error("the refused apply dropped the column anyway")
	}
}

// backdatePlanExpiry pushes a plan past its deadline without sleeping seven
// days. gateRequest.Now exists so expiry is decidable in a test; this is the
// other half of that.
func backdatePlanExpiry(t *testing.T, svc *Service, planID string) {
	t.Helper()
	tag, err := svc.pool.Exec(context.Background(), `
UPDATE atlantis.schema_plans SET expires_at = now() - interval '1 hour'
 WHERE plan_id = $1`, planID)
	if err != nil {
		t.Fatalf("backdate %s: %v", planID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdating %s updated %d rows, want 1", planID, tag.RowsAffected())
	}
}

// The superseded arm of the same fix, which nothing constrained.
//
// bedca6a re-opens a plan in THREE places — superseded, expired-approved, and
// diverged-approved — and shipped tests for two of them. Deleting the
// recordPendingPlan call from this arm left the whole admin package green,
// which is how a guard ends up present in the source and absent in effect.
//
// It is also the arm the commit is named after, and the one with the widest
// reach: supersedePlansFor marks every other pending or approved plan for a
// caller superseded on each apply, so any caller with two changes in flight
// reaches this state as a matter of course, not as an edge case.
func TestASupersededPlanReopensRatherThanBricking(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dssup", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dssup", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dssup", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("the destructive change applied without approval")
	}

	// Set directly, the same way the diverged test fakes divergence. Reaching
	// it through supersedePlansFor needs a second apply that moves the
	// checkpoint while leaving this plan's content current — a longer road to
	// the same row state, and it would be testing supersedePlansFor rather than
	// the arm below.
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE atlantis.schema_plans SET state = 'superseded' WHERE plan_id = $1`,
		plan.GetPlanId()); err != nil {
		t.Fatalf("supersede the plan: %v", err)
	}

	err := depScopeApply(t, svc, "dssup", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("a superseded plan applied with no fresh decision behind it")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "re-opened") {
		t.Errorf("the refusal does not tell the operator the request is live "+
			"again, so the next step is to wait for a decision nobody knows to "+
			"make: %v", err)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Errorf("plan state = %q, want %q", got, planPending)
	}

	// The consequence, which the state column alone does not establish. The id
	// is deterministic in (caller, files, dependency hash), so re-planning
	// returns this same id, and ApproveSchemaPlan only touches a row awaiting a
	// decision. Left superseded, the plan is one nobody can apply and nobody can
	// approve. approveStoredPlan fails this test unless exactly one pending row
	// matched, so it carries the assertion.
	approveStoredPlan(t, svc, plan.GetPlanId())
	if err := depScopeApply(t, svc, "dssup", "ledger.atl", gateLedgerV2, plan); err != nil {
		t.Errorf("the re-opened plan was approved and still would not apply, so "+
			"re-opening moved the row without making it usable: %v", err)
	}
}

// A pending request that outlived its TTL is renewed, not left to rot.
//
// The fifth recordPendingPlan site, and the last one with no test. Reached by
// the most ordinary sequence there is: a pipeline files a request, nobody
// decides within defaultPlanTTL, and the pipeline is re-run. Without the
// renewal the row keeps an expires_at in the past, and ApproveSchemaPlan —
// which will not act on an expired row — leaves the reviewer with a request
// they can see and cannot decide.
func TestAPendingPlanThatExpiredIsRenewed(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsren", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsren", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsren", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("the destructive change applied without approval")
	}
	// Pending, not approved, which separates this from
	// TestAnExpiredApprovalReopens, which backdates an approval instead.
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Fatalf("plan state = %q, want %q before backdating", got, planPending)
	}
	backdatePlanExpiry(t, svc, plan.GetPlanId())

	err := depScopeApply(t, svc, "dsren", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("the destructive change applied on the retry")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "renewed") {
		t.Errorf("the refusal does not say the request was renewed, so an operator "+
			"reading it cannot tell a live request from one that quietly aged out: %v", err)
	}

	// The renewal has to move expires_at, or "renewed" is a word in a message
	// and nothing else — the row stays undecidable and the next retry says the
	// same thing forever.
	var expiresAt time.Time
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT expires_at FROM atlantis.schema_plans WHERE plan_id = $1`,
		plan.GetPlanId()).Scan(&expiresAt); err != nil {
		t.Fatalf("read expires_at: %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expires_at is %s, still in the past — the request was reported "+
			"renewed and was not", expiresAt)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Errorf("plan state = %q, want %q", got, planPending)
	}
	// And it is decidable, which is what renewing it buys.
	approveStoredPlan(t, svc, plan.GetPlanId())
}

// An applied plan is terminal, and re-submitting it does not run the DDL twice.
//
// Untested until now: plans_pg_test.go names planApplied only as a
// ListSchemaPlans filter, never to drive the gate. The arm matters on a
// rollback-then-reapply, where the checkpoint moves back under a plan id that
// has already executed once.
func TestAnAppliedPlanIsNotReapplied(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsapp", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsapp", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsapp", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("the destructive change applied without approval")
	}
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE atlantis.schema_plans SET state = 'applied' WHERE plan_id = $1`,
		plan.GetPlanId()); err != nil {
		t.Fatalf("mark the plan applied: %v", err)
	}

	err := depScopeApply(t, svc, "dsapp", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("a plan recorded as already applied ran again. The DDL here drops " +
			"a column; running it twice is running a destructive change nobody " +
			"approved a second time")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "already been applied") {
		t.Errorf("the refusal does not say the plan was already applied: %v", err)
	}

	// Terminal means terminal: it must not have been re-opened. Re-opening here
	// would make an executed plan decidable again, which is an approval for
	// work that has already happened.
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planApplied {
		t.Errorf("plan state = %q, want %q — an applied plan that re-opens invites "+
			"a reviewer to approve DDL that already ran", got, planApplied)
	}
	// And the column survives, because the refused apply must not have run.
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

// A policy that cannot be read refuses everything, including what no rule gates.
//
// The failure this closes is the one the gate's own comment names: a dropped
// table or a revoked GRANT turning every gated class into an unattended apply.
// It is the hardest kind to notice, because nothing errors — applies simply
// stop being held.
//
// The probe is an ADDITIVE change, which 0026 seeds ungated. A refusal of this
// change cannot have come from a rule, so it can only have come from the read
// failing, which is what makes the assertion mean what it says.
func TestTheGateFailsClosedWhenThePolicyCannotBeRead(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsfail", "ledger.atl", gateLedgerV1)

	// Planned while the table is still readable: PlanSchema reports the rule for
	// the class it produced, so it needs the table too.
	plan := depScopePlan(t, svc, "dsfail", "ledger.atl", gateLedgerV3Additive)

	if _, err := svc.pool.Exec(context.Background(),
		`DROP TABLE atlantis.change_policy`); err != nil {
		t.Fatalf("drop the policy table: %v", err)
	}

	err := depScopeApply(t, svc, "dsfail", "ledger.atl", gateLedgerV3Additive, plan)
	if err == nil {
		t.Fatal("an apply proceeded with no readable change policy. Every gated " +
			"class is now an unattended apply, and nothing anywhere reports it")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("the refusal does not name the policy read as the cause, so an "+
			"operator cannot tell it from an ordinary approval requirement: %v", err)
	}

	// The additive column must not exist: fail-closed means the DDL did not run.
	var n int
	if qerr := svc.pool.QueryRow(context.Background(), `
SELECT count(*) FROM information_schema.columns
 WHERE table_schema = 'atlantis' AND table_name = 'dsgate_ledger' AND column_name = 'extra'`).Scan(&n); qerr != nil {
		t.Fatalf("check the column: %v", qerr)
	}
	if n != 0 {
		t.Error("the apply ran its DDL despite refusing, so the refusal is a " +
			"message rather than a gate")
	}
}

// TestADivergedApprovalReopensRatherThanBricking covers the state machine's
// missing edge.
//
// A plan id is deterministic in (caller, files, dependency hash). When the
// stored content diverged from what an apply would now run, the gate refused
// terminally and advised "re-plan and request approval again" — advice that
// cannot be followed, because re-planning returns the SAME id and the row is
// still `approved`, which ApproveSchemaPlan refuses as "not awaiting a
// decision". The plan is then both unapplicable and undecidable, and the only
// escape is editing the .atl bytes to move the hash, which no message mentions.
//
// A server upgrade that changes emitted SQL is enough to reach it. b6c0555
// added emitLockTimeout — exactly that shape — and would have bricked every
// approved-but-unapplied plan in a deployment.
func TestADivergedApprovalReopensRatherThanBricking(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsdiv", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsdiv", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsdiv", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("the destructive change applied without approval")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())

	// Divergence, standing in for a server upgrade that changes emitted SQL.
	if _, err := svc.pool.Exec(context.Background(),
		`UPDATE atlantis.schema_plans SET up_sql = up_sql || ' -- moved' WHERE plan_id = $1`,
		plan.GetPlanId()); err != nil {
		t.Fatalf("diverge the stored SQL: %v", err)
	}

	err := depScopeApply(t, svc, "dsdiv", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("an apply ran against an approval that was for different SQL")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}

	// The point of the fix: the row goes back to pending so somebody can
	// decide again.
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Errorf("plan state = %q, want %q — a diverged approval that stays "+
			"approved is a plan nobody can apply and nobody can approve, with "+
			"an id that re-planning reproduces exactly", got, planPending)
	}

	// And the stale decision is gone, or the console shows "approved by X"
	// beside a pending request.
	var decidedBy *string
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT decided_by FROM atlantis.schema_plans WHERE plan_id = $1`,
		plan.GetPlanId()).Scan(&decidedBy); err != nil {
		t.Fatalf("read decided_by: %v", err)
	}
	if decidedBy != nil && *decidedBy != "" {
		t.Errorf("decided_by is still %q on a re-opened request", *decidedBy)
	}
}

// TestAnExpiredApprovalReopens is the TTL, which nothing exercised.
//
// schemaPlan.Expired could be changed to `return false` and the whole package
// stayed green: no test wrote expires_at, and the 7-day default meant no plan
// created in a test was ever expired. That mutation removes every enforcement
// point at once and produces the exact failure the TTL's doc comment describes
// — "an approval granted in April and used in September".
func TestAnExpiredApprovalReopens(t *testing.T) {
	svc := depScopeService(t)
	depScopePlanAndApply(t, svc, "dsttl", "ledger.atl", gateLedgerV1)

	plan := depScopePlan(t, svc, "dsttl", "ledger.atl", gateLedgerV2)
	if err := depScopeApply(t, svc, "dsttl", "ledger.atl", gateLedgerV2, plan); err == nil {
		t.Fatal("the destructive change applied without approval")
	}
	approveStoredPlan(t, svc, plan.GetPlanId())

	backdatePlanExpiry(t, svc, plan.GetPlanId())

	err := depScopeApply(t, svc, "dsttl", "ledger.atl", gateLedgerV2, plan)
	if err == nil {
		t.Fatal("an expired approval was honoured — the TTL is the only thing " +
			"stopping a decision becoming a standing permission")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("refusal code = %s, want FailedPrecondition", got)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the refusal does not mention expiry, so the operator cannot "+
			"tell it from a rejection: %v", err)
	}
	if got := storedPlanState(t, svc, plan.GetPlanId()); got != planPending {
		t.Errorf("plan state = %q, want %q — an expired approval must re-open "+
			"for a fresh decision, not become undecidable", got, planPending)
	}

	// The column survives.
	//
	// dsgate_ledger, not dsttl_ledger: the physical table is named from the
	// ENTITY's namespace, and gateLedgerV1 declares `entity Ledger in dsgate`
	// whatever caller submits it. Querying dsttl_ledger returned 0 and read as
	// "the apply dropped the column" when nothing of the sort had happened —
	// an assertion against a table that never existed.
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
