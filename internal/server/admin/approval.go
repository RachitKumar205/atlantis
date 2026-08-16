package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// Plan states. Text rather than an enum column, and unconstrained in the
// schema, for the reason 0018 gives about capability names — but the set is
// closed here, and anything outside it is treated as "not approved".
const (
	planPending    = "pending_approval"
	planApproved   = "approved"
	planRejected   = "rejected"
	planApplied    = "applied"
	planSuperseded = "superseded"
)

// defaultPlanTTL is how long a pending or approved plan stays actionable.
//
// An approval is a statement about a schema someone read at a particular
// moment. Left open-ended it becomes a standing permission that outlives the
// reasoning behind it — the classic shape being an approval granted in April
// and used in September against a schema that has moved underneath it in every
// way except the ones the token happens to cover.
const defaultPlanTTL = 7 * 24 * time.Hour

// gateOnChangePolicy decides whether this change may apply now, and records the
// request when it may not.
//
// # Why this is in the handler and not an interceptor
//
// An interceptor sees the method name and the connection. It never sees the
// request body, and "which class of change is this" is a property of the
// submitted files — it is not knowable until they have been parsed, lowered,
// diffed and emitted. Same category as bindCallerIdentity, and documented there
// at length for the same reason.
//
// The name deliberately avoids the looksLikeAuthz prefixes that
// handler_authz_test.go scans for. This is not an authorization decision: the
// caller's right to apply was settled by the interceptor before any of this
// ran. This asks a different question — whether what they are applying is the
// kind of thing a human has to see first.
//
// # Why it runs inside the caller's transaction
//
// The policy read and the plan-state read both happen on tx, under the advisory
// lock the apply already holds. That is what serialises this against a
// concurrent policy edit or a concurrent approval: a rule tightened halfway
// through an apply either lands before this reads it or waits behind the lock,
// and never splits the difference.
//
// # Why the decision reads tx but the record is written on the pool
//
// The decision has to be serialised, so it reads under the advisory lock the
// apply already holds. The RECORD must not be: a refusal returns an error, the
// apply transaction rolls back, and anything written inside it goes with it —
// so the request an operator is supposed to approve would vanish at the exact
// moment it started to matter. The first version of this did precisely that,
// and the tests found a gate that refused correctly and left nothing behind.
//
// The two are safe to split. Nothing else writes schema_plans, and the upsert
// refuses to touch a row that is already approved or rejected, so the worst a
// race can do is rewrite a pending row with the same content.
//
// Returns nil when the apply may proceed. Otherwise the error carries
// codes.FailedPrecondition so a client can branch on the code rather than on
// message text.
func (s *Service) gateOnChangePolicy(ctx context.Context, tx pgx.Tx, g gateRequest) error {
	// EVERY class the diff contains, not just the worst one.
	//
	// The policy is four independent rules. Consulting only the highest class
	// meant a diff that both renamed a table and dropped a column asked the
	// breaking rule and never the destructive one — so with breaking set to
	// auto-apply, the DROP ran unattended and no plan row was ever filed. The
	// operator had explicitly required approval for destructive changes.
	present := g.Diff.ClassesPresent()
	if len(present) == 0 {
		// An empty diff has nothing to gate. Reaching here means the caller
		// asked to apply nothing, which the apply path handles above.
		return nil
	}

	stored, _, err := loadChangePolicy(ctx, tx)
	if err != nil {
		// Fail closed. A policy that cannot be read is not a policy that
		// permits: the alternative is that a dropped table or a revoked GRANT
		// turns every gated class into an unattended apply, which is the exact
		// failure the gate exists to prevent and the hardest kind to notice.
		return status.Errorf(codes.FailedPrecondition,
			"admin: the change policy could not be read, so no change may apply unattended: %v", err)
	}

	// ClassesPresent is ordered most-restrictive first, so the first class that
	// requires approval is the one to record and to name in the refusal. That
	// keeps the plan filed under the rule that actually stopped it rather than
	// under whichever class happened to be worst.
	var class adminpb.PlanClass
	var policy ChangePolicy
	var gated bool
	for _, c := range present {
		pb := planClassToPB(translateClass(c))
		// A class outside the settable set cannot be governed by a rule, and
		// there are only two: UNSPECIFIED, meaning the diff produced nothing a
		// class maps to, and UNPARSEABLE, meaning the DSL did not compile and
		// the apply was refused long before here. Neither may proceed quietly.
		//
		// UNREACHABLE as written, and deliberately kept. policyClassIsSettable
		// is defined as the image of ChangeClasses(), and ClassesPresent()
		// returns those same four values, so pb is always settable. There is
		// therefore no test that drives this arm — one would have to fabricate a
		// state the code cannot produce. TestEveryClassPresentIsSettable pins
		// the invariant instead, and is what would fail if a fifth bucket
		// arrived in ClassesPresent without a matching entry in ChangeClasses.
		// Keep the arm: it costs nothing, it fails closed, and the day the
		// invariant breaks is the day it stops being unreachable.
		if !policyClassIsSettable(pb) {
			return status.Errorf(codes.FailedPrecondition,
				"admin: plan class %s cannot be evaluated against the change policy", pb)
		}
		if p := effectiveChangePolicy(stored, pb); p.RequireApproval {
			class, policy, gated = pb, p, true
			break
		}
	}
	if !gated {
		return nil
	}

	// The rule says a human must decide. Find out whether one has.
	plan, found, err := loadSchemaPlan(ctx, tx, g.PlanID)
	if err != nil {
		return fmt.Errorf("load plan %s: %w", g.PlanID, err)
	}

	switch {
	case !found:
		if err := s.recordPendingPlan(ctx, g, class); err != nil {
			return fmt.Errorf("record plan %s: %w", g.PlanID, err)
		}
		return approvalRequired(g.PlanID, class, policy.ApproverRole,
			"recorded and waiting for a decision")

	case plan.State == planRejected:
		// Terminal. Re-submitting must not quietly re-open it, or "rejected"
		// means "rejected until the pipeline retries in five minutes".
		return status.Errorf(codes.FailedPrecondition,
			"admin: plan %s was rejected by %s%s — change the schema and re-plan; "+
				"resubmitting the same change does not re-open the request",
			g.PlanID, plan.DecidedBy, reasonSuffix(plan.DecisionReason))

	case plan.State == planApplied:
		return status.Errorf(codes.FailedPrecondition,
			"admin: plan %s has already been applied", g.PlanID)

	case plan.State == planSuperseded:
		// Re-opened, not merely reported. A superseded plan whose content is
		// current again is a live request, and leaving it superseded made the
		// id permanently undecidable: ApproveSchemaPlan refuses anything not
		// awaiting a decision, and the plan id is deterministic in (caller,
		// files, dependency hash), so re-planning returns the SAME dead id.
		// The only escape was editing the .atl bytes to move the hash, which
		// no message suggests and no reader would guess.
		if err := s.recordPendingPlan(ctx, g, class); err != nil {
			return fmt.Errorf("re-open superseded plan %s: %w", g.PlanID, err)
		}
		return approvalRequired(g.PlanID, class, policy.ApproverRole,
			"the previous request was superseded by a change to the schema it "+
				"depended on, and has been re-opened for a decision")

	case plan.State == planApproved:
		if plan.Expired(g.Now) {
			// Same reasoning: void the approval AND re-open, so the expiry is
			// a renewal rather than a dead end.
			if err := s.recordPendingPlan(ctx, g, class); err != nil {
				return fmt.Errorf("re-open expired plan %s: %w", g.PlanID, err)
			}
			return approvalRequired(g.PlanID, class, policy.ApproverRole,
				"the approval expired before it was used and has been re-opened")
		}
		// An approval is for a specific change. Re-derive what would run now
		// and require it to match what was approved.
		//
		// Both the diff and the SQL are compared, and the second is not
		// redundant: an identical structural diff can emit different SQL when
		// the prior IR moved underneath it, and the SQL is what actually
		// executes. Comparing only the diff would approve a shape and run a
		// script.
		//
		// Divergence VOIDS the approval and re-opens the request. It used to
		// refuse terminally, and the advice it gave — "re-plan and request
		// approval again" — was impossible to follow, because re-planning
		// produces the identical id and the row stayed `approved`. A server
		// upgrade that changes emitted SQL is enough to reach this: b6c0555
		// added emitLockTimeout, which is exactly that shape, and it would
		// have bricked every approved-but-unapplied plan in the deployment.
		if plan.FilesHash != g.FilesHash || plan.UpSQL != g.UpSQL {
			what := "the SQL this apply would run"
			if plan.FilesHash != g.FilesHash {
				what = "the submitted file contents"
			}
			if err := s.recordPendingPlan(ctx, g, class); err != nil {
				return fmt.Errorf("re-open diverged plan %s: %w", g.PlanID, err)
			}
			return approvalRequired(g.PlanID, class, policy.ApproverRole,
				"the approval was for a different version of "+what+
					", so it no longer applies and the request has been re-opened")
		}
		return nil

	default:
		// planPending, or a state this build does not recognise. Both mean
		// "nobody has said yes".
		if plan.Expired(g.Now) {
			if err := s.recordPendingPlan(ctx, g, class); err != nil {
				return fmt.Errorf("refresh plan %s: %w", g.PlanID, err)
			}
			return approvalRequired(g.PlanID, class, policy.ApproverRole,
				"the previous request expired and has been renewed")
		}
		return approvalRequired(g.PlanID, class, policy.ApproverRole,
			"waiting for a decision")
	}
}

// gateRequest is everything the gate needs about one attempted apply.
//
// Now is passed in rather than read from the clock so expiry is decided once
// per apply, by the caller, and a test can place a plan either side of its
// deadline without sleeping.
type gateRequest struct {
	Caller      string
	PlanID      string
	RequestedBy string
	Files       []SubmittedFile
	FilesHash   string
	BaseHash    string
	Diff        *codegen.Diff
	UpSQL       string
	DownSQL     string
	Now         time.Time
}

// approvalRequired is the refusal a caller sees when a human has to decide.
//
// FailedPrecondition, so `tide apply` branches on the code. The message names
// the role because "someone must approve this" without saying who leaves the
// engineer to go and ask which channel to post in.
func approvalRequired(planID string, class adminpb.PlanClass, role, detail string) error {
	return status.Errorf(codes.FailedPrecondition,
		"admin: this change is %s and needs approval from %s before it can apply. "+
			"Plan %s: %s.", humanClass(class), role, planID, detail)
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// humanClass renders a class the way the console and `tide plan` do, so the
// three surfaces name the same thing the same way.
func humanClass(c adminpb.PlanClass) string {
	switch c {
	case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
		return "additive"
	case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
		return "backfill-required"
	case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
		return "cross-caller-breaking"
	case adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE:
		return "destructive"
	}
	return c.String()
}

// schemaPlan is one row of atlantis.schema_plans.
type schemaPlan struct {
	PlanID         string
	Caller         string
	ChangeClass    string
	FilesHash      string
	BaseHash       string
	UpSQL          string
	State          string
	RequestedBy    string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	DecidedBy      string
	DecidedByRole  string
	DecidedAt      *time.Time
	DecisionReason string

	// Diff is the stored diff as written, kept raw. Nothing on the read path
	// needs the decoded value except planEntityIDs, and holding it as bytes
	// means a row this build cannot fully decode still lists.
	Diff []byte
}

// Expired reports whether this plan's window has closed.
//
// Evaluated on every read rather than by a sweeper, so nothing about
// correctness depends on a background job having run. A sweeper that stops —
// and they do — would otherwise leave approvals silently valid forever, which
// is the failure mode this whole file exists to make impossible.
func (p schemaPlan) Expired(now time.Time) bool {
	return p.ExpiresAt != nil && now.After(*p.ExpiresAt)
}

func loadSchemaPlan(ctx context.Context, q pgxQuerier, planID string) (schemaPlan, bool, error) {
	rows, err := q.Query(ctx, `
SELECT plan_id, caller, change_class, files_hash, base_checkpoint_hash, up_sql,
       state, requested_by, created_at, expires_at, decided_by, decided_by_role,
       decided_at, decision_reason
FROM atlantis.schema_plans WHERE plan_id = $1`, planID)
	if err != nil {
		return schemaPlan{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return schemaPlan{}, false, rows.Err()
	}
	var p schemaPlan
	if err := rows.Scan(&p.PlanID, &p.Caller, &p.ChangeClass, &p.FilesHash, &p.BaseHash,
		&p.UpSQL, &p.State, &p.RequestedBy, &p.CreatedAt, &p.ExpiresAt,
		&p.DecidedBy, &p.DecidedByRole, &p.DecidedAt, &p.DecisionReason); err != nil {
		return schemaPlan{}, false, err
	}
	return p, true, rows.Err()
}

// recordPendingPlan records a request for approval, on its own connection.
//
// Deliberately s.pool and not the apply's transaction. See the note on
// gateOnChangePolicy: this is written on a path that is about to return an
// error, and the caller's transaction is about to roll back.
//
// # What the WHERE clause on the update arm actually does
//
// It is a CONCURRENCY backstop, not the thing that stops a retrying pipeline
// from undoing an approval. That distinction was wrong here for a while, and
// the comment asserting it survived a test that named it — because no
// single-threaded path reaches the upsert with a row in a state the clause
// excludes. gateOnChangePolicy returns before calling this for a rejected plan,
// for an applied one, and for an approved one whose content still matches.
//
// What protects the sequential retry is that early return. What the clause
// protects is the interleaving: two applies racing on the same plan id while an
// operator decides. `rejected` and `applied` are terminal and excluded, so a
// concurrent write cannot resurrect either.
//
// `approved` is INCLUDED, and deliberately. The approved arm calls this only
// when the stored content has diverged from what would now run — the approval
// is void at that point, and the row has to go back to pending or the plan id
// becomes permanently undecidable. Excluding it is what made a server upgrade
// that changed emitted SQL brick every approved-but-unapplied plan, with an
// error advising a re-plan that returns the same dead id.
func (s *Service) recordPendingPlan(ctx context.Context, g gateRequest, class adminpb.PlanClass) error {
	filesJSON, err := json.Marshal(readableFiles(g.Files))
	if err != nil {
		return fmt.Errorf("marshal files: %w", err)
	}
	diffJSON, err := json.Marshal(g.Diff)
	if err != nil {
		return fmt.Errorf("marshal diff: %w", err)
	}
	expires := g.Now.Add(defaultPlanTTL)

	_, err = s.pool.Exec(ctx, `
INSERT INTO atlantis.schema_plans
    (plan_id, caller, change_class, files, files_hash, diff, up_sql, down_sql,
     base_checkpoint_hash, state, requested_by, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (plan_id) DO UPDATE SET
    files                = EXCLUDED.files,
    files_hash           = EXCLUDED.files_hash,
    diff                 = EXCLUDED.diff,
    up_sql               = EXCLUDED.up_sql,
    down_sql             = EXCLUDED.down_sql,
    base_checkpoint_hash = EXCLUDED.base_checkpoint_hash,
    state                = EXCLUDED.state,
    requested_by         = EXCLUDED.requested_by,
    created_at           = EXCLUDED.created_at,
    expires_at           = EXCLUDED.expires_at,
    -- Cleared, because the row is going back to pending and a decision that
    -- no longer applies must not be displayed beside it. Leaving these would
    -- show a re-opened request as "approved by X" in the console while the
    -- state column says otherwise.
    --
    -- Empty string rather than NULL for the three TEXT columns: 0027 declares
    -- them NOT NULL DEFAULT ''. decided_at is the one that is nullable, and
    -- NULL is right there — "no decision at this time" is exactly what a
    -- re-opened request means.
    decided_by           = '',
    decided_by_role      = '',
    decided_at           = NULL,
    decision_reason      = ''
WHERE atlantis.schema_plans.state IN ('pending_approval', 'superseded', 'approved')`,
		g.PlanID, g.Caller, class.String(), filesJSON, g.FilesHash, diffJSON,
		g.UpSQL, g.DownSQL, g.BaseHash, planPending, g.RequestedBy, g.Now, expires)
	return err
}

// readableFiles renders the submitted set for storage as text rather than as
// SubmittedFile would marshal it.
//
// SubmittedFile.Content is []byte, and encoding/json writes []byte as base64.
// Stored that way the column is unreadable in psql, which is where somebody
// ends up during an incident, and it is contrary to how the rest of this schema
// is built — enum names rather than numbers, for exactly the same reason. .atl
// source is text; storing it as text costs nothing and keeps the row legible.
func readableFiles(files []SubmittedFile) []struct {
	Path    string `json:"path"`
	Content string `json:"content"`
} {
	out := make([]struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}, 0, len(files))
	for _, f := range files {
		out = append(out, struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}{Path: f.Path, Content: string(f.Content)})
	}
	return out
}

// markPlanApplied closes out a plan whose apply succeeded.
//
// Called with the version the apply produced, so an auditor reading a plan row
// can reach the schema_versions row it became without correlating timestamps.
func markPlanApplied(ctx context.Context, tx pgx.Tx, planID string, version int64) error {
	_, err := tx.Exec(ctx, `
UPDATE atlantis.schema_plans
   SET state = $2, applied_version = $3
 WHERE plan_id = $1 AND state IN ('approved', 'pending_approval')`,
		planID, planApplied, version)
	return err
}

// supersedePlansFor marks a caller's outstanding requests as no longer
// answering the question they were filed about.
//
// Called after a successful apply by the same caller. A plan left behind was
// computed against a checkpoint that has now moved; approving one would be
// approving a diff that no longer describes anything. Superseded rather than
// deleted, so the console can show what happened to a request somebody was
// waiting on rather than having it vanish.
//
// # Only when the checkpoint really moved
//
// newCheckpointHash gates it, because the premise above is not always true. An
// apply that changes no schema still runs: a comment-only edit produces
// different file bytes, an identical IR and an empty diff, which classifies
// additive and passes ungated. Retiring every outstanding plan on the strength
// of that retired requests that were still perfectly valid — and because the
// plan id is deterministic in (caller, files, dependency hash), the caller
// could not file a fresh one either. The id came back identical and already
// superseded.
//
// IS DISTINCT FROM rather than <>, so a row with no recorded base hash is
// superseded rather than skipped. Not knowing what a plan was computed against
// is a reason to retire it, not a reason to keep it.
func supersedePlansFor(ctx context.Context, tx pgx.Tx, caller, keepPlanID, newCheckpointHash string) error {
	_, err := tx.Exec(ctx, `
UPDATE atlantis.schema_plans
   SET state = $3
 WHERE caller = $1
   AND plan_id <> $2
   AND state IN ('pending_approval', 'approved')
   AND base_checkpoint_hash IS DISTINCT FROM $4`,
		caller, keepPlanID, planSuperseded, newCheckpointHash)
	return err
}
