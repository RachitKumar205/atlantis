package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// The decision surface: reading what is waiting, and saying yes or no.
//
// Kept apart from approval.go, which is the gate. The gate runs on the apply
// path and answers "may this proceed"; this runs on the console's path and
// answers "what is waiting, and here is a human's answer". They share the table
// and nothing else, and a change to one should not need a reading of the other.

// planListLimit bounds a list that has no natural end. The console paginates
// nothing today, and a deployment that accumulated ten thousand superseded
// plans would otherwise serve all of them into a browser.
const planListLimit = 200

// ListSchemaPlans reports the plans matching a state, newest first.
func (s *Service) ListSchemaPlans(ctx context.Context, req *adminpb.ListSchemaPlansRequest) (*adminpb.ListSchemaPlansResponse, error) {
	if s.pool == nil {
		return &adminpb.ListSchemaPlansResponse{}, nil
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > planListLimit {
		limit = planListLimit
	}

	// Empty state and empty caller mean "no filter". Expressed as SQL rather
	// than as branching query strings so there is one statement to read.
	rows, err := s.pool.Query(ctx, `
SELECT plan_id, caller, change_class, files_hash, base_checkpoint_hash, up_sql,
       state, requested_by, created_at, expires_at, decided_by, decided_by_role,
       decided_at, decision_reason, diff, requested_by_actor, decided_via,
       rehearsal_id, verdict, requested_by_actor_email, requested_by_actor_name
FROM atlantis.schema_plans
WHERE ($1 = '' OR state = $1)
  AND ($2 = '' OR caller = $2)
ORDER BY created_at DESC
LIMIT $3`, req.GetState(), req.GetCaller(), limit)
	if err != nil {
		return nil, fmt.Errorf("list schema plans: %w", err)
	}
	defer rows.Close()

	var plans []schemaPlan
	for rows.Next() {
		var p schemaPlan
		if err := rows.Scan(&p.PlanID, &p.Caller, &p.ChangeClass, &p.FilesHash, &p.BaseHash,
			&p.UpSQL, &p.State, &p.RequestedBy, &p.CreatedAt, &p.ExpiresAt,
			&p.DecidedBy, &p.DecidedByRole, &p.DecidedAt, &p.DecisionReason, &p.Diff,
			&p.RequestedByActor, &p.DecidedVia, &p.RehearsalID, &p.Verdict,
			&p.RequestedByActorEmail, &p.RequestedByActorName); err != nil {
			return nil, fmt.Errorf("scan schema plan: %w", err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema plans: %w", err)
	}

	stored, _, err := loadChangePolicy(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]*adminpb.SchemaPlanSummary, 0, len(plans))
	for _, p := range plans {
		out = append(out, planSummaryToPB(p, stored, now))
	}
	return &adminpb.ListSchemaPlansResponse{Plans: out}, nil
}

// GetSchemaPlan returns everything a reviewer needs to decide.
func (s *Service) GetSchemaPlan(ctx context.Context, req *adminpb.GetSchemaPlanRequest) (*adminpb.GetSchemaPlanResponse, error) {
	if req.GetPlanId() == "" {
		return nil, status.Error(codes.InvalidArgument, "admin: plan_id is required")
	}
	if s.pool == nil {
		return nil, status.Error(codes.NotFound, "admin: no such plan")
	}

	var (
		p         schemaPlan
		filesJSON []byte
		downSQL   string
	)
	err := s.pool.QueryRow(ctx, `
SELECT plan_id, caller, change_class, files_hash, base_checkpoint_hash, up_sql, down_sql,
       state, requested_by, created_at, expires_at, decided_by, decided_by_role,
       decided_at, decision_reason, files, diff, requested_by_actor, decided_via,
       rehearsal_id, verdict, requested_by_actor_email, requested_by_actor_name
FROM atlantis.schema_plans WHERE plan_id = $1`, req.GetPlanId()).Scan(
		&p.PlanID, &p.Caller, &p.ChangeClass, &p.FilesHash, &p.BaseHash, &p.UpSQL, &downSQL,
		&p.State, &p.RequestedBy, &p.CreatedAt, &p.ExpiresAt, &p.DecidedBy, &p.DecidedByRole,
		&p.DecidedAt, &p.DecisionReason, &filesJSON, &p.Diff, &p.RequestedByActor, &p.DecidedVia,
		&p.RehearsalID, &p.Verdict, &p.RequestedByActorEmail, &p.RequestedByActorName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "admin: no plan %s", req.GetPlanId())
		}
		return nil, fmt.Errorf("load schema plan: %w", err)
	}

	stored, _, err := loadChangePolicy(ctx, s.pool)
	if err != nil {
		return nil, err
	}

	var storedFiles []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(filesJSON, &storedFiles); err != nil {
		return nil, fmt.Errorf("decode stored files for %s: %w", p.PlanID, err)
	}
	files := make([]*adminpb.SubmittedFile, 0, len(storedFiles))
	for _, f := range storedFiles {
		files = append(files, &adminpb.SubmittedFile{Path: f.Path, Content: []byte(f.Content)})
	}

	return &adminpb.GetSchemaPlanResponse{Plan: &adminpb.SchemaPlanDetail{
		Summary:            planSummaryToPB(p, stored, time.Now().UTC()),
		Files:              files,
		Diff:               p.Diff,
		UpSql:              p.UpSQL,
		DownSql:            downSQL,
		BaseCheckpointHash: p.BaseHash,
	}}, nil
}

// ApproveSchemaPlan records a human saying yes.
func (s *Service) ApproveSchemaPlan(ctx context.Context, req *adminpb.ApproveSchemaPlanRequest) (*adminpb.ApproveSchemaPlanResponse, error) {
	p, err := s.decide(ctx, decision{
		PlanID:   req.GetPlanId(),
		By:       req.GetDecidedBy(),
		Role:     req.GetDecidedByRole(),
		Reason:   req.GetReason(),
		NewState: planApproved,
		Actor:    req.GetDecidedByActor(),
		Override: req.GetOverride(),
	})
	if err != nil {
		return nil, err
	}
	return &adminpb.ApproveSchemaPlanResponse{Plan: p}, nil
}

// RejectSchemaPlan records a human saying no, and why.
func (s *Service) RejectSchemaPlan(ctx context.Context, req *adminpb.RejectSchemaPlanRequest) (*adminpb.RejectSchemaPlanResponse, error) {
	// A rejection with no reason is a wall. The caller sees this text and
	// nothing else on their next apply, so "no" without it leaves them
	// re-running the same command to find out what to change.
	if req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument,
			"admin: a rejection needs a reason — it is the only thing the caller is told")
	}
	p, err := s.decide(ctx, decision{
		PlanID:   req.GetPlanId(),
		By:       req.GetDecidedBy(),
		Role:     req.GetDecidedByRole(),
		Reason:   req.GetReason(),
		NewState: planRejected,
	})
	if err != nil {
		return nil, err
	}
	return &adminpb.RejectSchemaPlanResponse{Plan: p}, nil
}

type decision struct {
	PlanID   string
	By       string
	Role     string
	Reason   string
	NewState string

	// Actor is the decider's principal (console:<subject>), compared against
	// the plan's requested_by_actor for the self-approval refusal.
	Actor string

	// Override marks an approval with extra ceremony: it waives the
	// self-approval refusal, a protected-entity floor's wait, and any freeze
	// window at apply time — never a capability, the sandbox_only tier, or
	// the plan's content binding. Requires a reason and the admin role.
	Override bool
}

// decide is the shared body of approve and reject.
//
// One function because the two differ in the state they write and nothing else
// that matters — and because every check below has to apply to both. A reject
// that skipped the role check would let anyone who can reach the RPC block
// every deploy in the deployment, which is a denial of service wearing the
// clothes of a safety feature.
func (s *Service) decide(ctx context.Context, d decision) (*adminpb.SchemaPlanSummary, error) {
	if d.PlanID == "" {
		return nil, status.Error(codes.InvalidArgument, "admin: plan_id is required")
	}
	if d.By == "" {
		return nil, status.Error(codes.InvalidArgument,
			"admin: decided_by is required — an unattributed decision is not an audit record")
	}
	if d.Override {
		// An override is an approval with extra ceremony, never a bypass.
		if d.NewState != planApproved {
			return nil, status.Error(codes.InvalidArgument,
				"admin: only an approval can be an override")
		}
		if d.Reason == "" {
			return nil, status.Error(codes.InvalidArgument,
				"admin: an override needs a reason — it is the record of why the gate was waived")
		}
		if d.Role != "admin" {
			return nil, status.Error(codes.PermissionDenied,
				"admin: overrides are decided by admin, and this decision asserts "+d.Role)
		}
	}
	if err := s.requireMutablePlane("schema plan decision"); err != nil {
		return nil, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The advisory lock the apply path takes, so a decision cannot land in the
	// middle of an apply reading the same row. Without it an approval could be
	// written between the gate's read and the apply's commit, and the apply
	// would refuse a plan that was approved a millisecond earlier — reported to
	// the operator as the product ignoring them.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(0x70636661706c79)); err != nil {
		return nil, fmt.Errorf("advisory lock: %w", err)
	}

	plan, found, err := loadSchemaPlan(ctx, tx, d.PlanID)
	if err != nil {
		return nil, fmt.Errorf("load plan %s: %w", d.PlanID, err)
	}
	if !found {
		return nil, status.Errorf(codes.NotFound, "admin: no plan %s", d.PlanID)
	}
	if plan.State != planPending {
		return nil, status.Errorf(codes.FailedPrecondition,
			"admin: plan %s is %s, not awaiting a decision", d.PlanID, plan.State)
	}
	now := time.Now().UTC()
	if plan.Expired(now) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"admin: plan %s expired on %s — the caller must re-plan and request again",
			d.PlanID, plan.ExpiresAt.UTC().Format(time.RFC3339))
	}

	// The role the asserting client claims must be the role the policy names.
	//
	// This cannot prove the console checked honestly — console roles are the
	// console's identity system and this server has no view of them. What it
	// can do is refuse an assertion that does not match its own rule, so the
	// console's middleware is not the only thing between a viewer and an
	// approval. A middleware ordering bug then produces a refusal here rather
	// than a decision nobody was entitled to make.
	stored, _, err := loadChangePolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	class := planClassFromName(plan.ChangeClass)
	policy := effectiveChangePolicy(stored, class)

	// A protected entity's admin_only floor overrides who may decide,
	// whatever role the class rule names. Matched from the stored diff;
	// undecodable rows fail toward requiring an admin.
	prot, err := s.decideProtectedFloor(ctx, tx, plan)
	if err != nil {
		return nil, err
	}
	if prot != nil && prot.AdminOnly() {
		policy.ApproverRole = "admin"
	}

	// An admin's override satisfies the role rule — the override ceremony
	// already demanded admin — but an ordinary decision must assert exactly
	// the role the rule names.
	if d.Role != policy.ApproverRole && !(d.Override && d.Role == "admin") {
		return nil, status.Errorf(codes.PermissionDenied,
			"admin: %s changes are decided by %s, and this decision asserts %q",
			humanClass(class), policy.ApproverRole, d.Role)
	}

	// The requester cannot be the approver. Soft-mandatory: the actor is
	// unauthenticated attribution, so this stops the honest loop — a person
	// approving the change they themselves submitted — and an override with
	// a reason passes it, loudly. An empty actor never matches.
	decidedVia := ""
	if d.Override {
		decidedVia = "override"
	}
	if d.NewState == planApproved && plan.RequestedByActor != "" &&
		normalizeActor(d.Actor) == plan.RequestedByActor {
		if !d.Override {
			return nil, status.Errorf(codes.PermissionDenied,
				"admin: this plan was requested by %s, and a request is not approved by "+
					"the person who made it. Another %s can approve it, or an admin can "+
					"override with a reason.", plan.RequestedByActor, policy.ApproverRole)
		}
		decidedVia = "self_approval_override"
	}

	if _, err := tx.Exec(ctx, `
UPDATE atlantis.schema_plans
   SET state = $2, decided_by = $3, decided_by_role = $4, decided_at = $5,
       decision_reason = $6, decided_via = $8
 WHERE plan_id = $1 AND state = $7`,
		d.PlanID, d.NewState, d.By, d.Role, now, d.Reason, planPending, decidedVia); err != nil {
		return nil, fmt.Errorf("record decision on %s: %w", d.PlanID, err)
	}

	if decidedVia != "" {
		if err := s.recordPolicyEvent(ctx, tx, "plan_override", map[string]string{
			"plan_id": d.PlanID, "caller": plan.Caller, "class": plan.ChangeClass,
			"decided_via": decidedVia, "reason": d.Reason,
		}, normalizeActor(d.Actor), d.By); err != nil {
			return nil, err
		}
	}

	updated, _, err := loadSchemaPlan(ctx, tx, d.PlanID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return planSummaryToPB(updated, stored, now), nil
}

// decideProtectedFloor answers whether this plan's diff touches a protected
// entity. From the stored diff — decide has no live one — failing toward
// admin_only when the row cannot say what it touches.
func (s *Service) decideProtectedFloor(ctx context.Context, tx pgx.Tx, p schemaPlan) (*protectedRule, error) {
	rules, err := loadProtectedEntities(ctx, tx)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"admin: the protected entities could not be read, so no plan can be decided: %v", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	ids := planEntityIDs(p.Diff)
	if len(ids) == 0 {
		return &protectedRule{Pattern: "(unattributable diff)", Floor: "admin_only",
			Reason: "the stored diff names no entities, so protection cannot be matched"}, nil
	}
	return matchProtected(rules, ids), nil
}

// planClassFromName resolves a stored class name.
//
// An unrecognised name yields UNSPECIFIED, which effectiveChangePolicy resolves
// to "requires approval by the default role" — so a row written by a future
// build is decidable by an admin rather than by nobody.
func planClassFromName(name string) adminpb.PlanClass {
	if num, ok := adminpb.PlanClass_value[name]; ok {
		return adminpb.PlanClass(num)
	}
	return adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED
}

// planEntityIDs reports the entities a stored diff touches, sorted and without
// duplicates.
//
// Derived from the DIFF, not from the submitted files. The files are what the
// caller wrote; the diff is what the apply will do, and those differ whenever a
// file is edited without changing the schema it declares. A strip promising
// "changes in flight for this entity" has to name the second.
//
// Reads Diff.All() rather than the four buckets by name, for the reason
// countDiffChanges gives: a local struct listing the buckets is how one gets
// missed, and the bucket that went missing was the destructive one. All() is
// exhaustive and TestDiffAllCoversEveryBucket keeps it that way, so a fifth
// bucket is covered here the day it exists rather than the day someone
// remembers this function.
//
// Tolerant of a diff that will not decode, matching countDiffChanges beside it.
// This is not the guard against a renamed bucket — JSON decoding is tolerant, so
// a renamed bucket decodes to empty and returns no error at all. What guards
// that is TestPlanEntityIDsReadsEveryBucket, which builds a change in every
// bucket by reflection.
func planEntityIDs(diffRaw []byte) []string {
	var d codegen.Diff
	if json.Unmarshal(diffRaw, &d) != nil {
		return nil
	}
	changes := d.All()
	seen := make(map[string]struct{}, len(changes))
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		if ch.EntityID == "" {
			continue
		}
		if _, dup := seen[ch.EntityID]; dup {
			continue
		}
		seen[ch.EntityID] = struct{}{}
		out = append(out, ch.EntityID)
	}
	sort.Strings(out)
	return out
}

func planSummaryToPB(p schemaPlan, stored map[adminpb.PlanClass]ChangePolicy, now time.Time) *adminpb.SchemaPlanSummary {
	class := planClassFromName(p.ChangeClass)
	out := &adminpb.SchemaPlanSummary{
		PlanId:         p.PlanID,
		Caller:         p.Caller,
		ChangeClass:    class,
		State:          p.State,
		RequestedBy:    p.RequestedBy,
		CreatedAt:      p.CreatedAt.UTC().Format(time.RFC3339),
		DecidedBy:      p.DecidedBy,
		DecidedByRole:  p.DecidedByRole,
		DecisionReason: p.DecisionReason,
		ApproverRole:   effectiveChangePolicy(stored, class).ApproverRole,
		Expired:        p.Expired(now),
		EntityIds:      planEntityIDs(p.Diff),

		RequestedByActor: p.RequestedByActor,
		DecidedVia:       p.DecidedVia,
		RehearsalId:      p.RehearsalID,
		RehearsalVerdict: p.Verdict,

		RequestedByActorEmail: p.RequestedByActorEmail,
		RequestedByActorName:  p.RequestedByActorName,
	}
	if p.ExpiresAt != nil {
		out.ExpiresAt = p.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if p.DecidedAt != nil {
		out.DecidedAt = p.DecidedAt.UTC().Format(time.RFC3339)
	}
	return out
}
