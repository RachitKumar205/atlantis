package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// applyPolicy is a caller's tier: how much of what it submits may apply
// without a human. The change policy is the deployment-wide floor per plan
// class; this is the per-caller ceiling, and the gate takes the most
// restrictive answer of the two.
type applyPolicy string

const (
	// PolicySandboxOnly refuses every apply and backfill outright. The caller
	// keeps plan, generate and rehearse.
	PolicySandboxOnly applyPolicy = "sandbox_only"

	// PolicyAlwaysAsk queues every class for approval.
	PolicyAlwaysAsk applyPolicy = "always_ask"

	// PolicyAutoSafe applies additive changes unattended and queues the rest.
	// What '' in caller_identities.apply_policy resolves to.
	PolicyAutoSafe applyPolicy = "auto_safe"

	// PolicyAutoVerified additionally applies a backfill-required change
	// unattended when a fresh rehearsal verdict passes.
	PolicyAutoVerified applyPolicy = "auto_verified"

	// PolicyAutoAll applies destructive changes too, behind the same verdict.
	// Cross-caller-breaking still queues: a per-caller policy cannot consent
	// on behalf of the other callers the change breaks.
	PolicyAutoAll applyPolicy = "auto_all"
)

// applyPolicyRank orders the tiers by how much they let through, for clamping
// against the floor. Unknown values rank lowest.
func applyPolicyRank(p applyPolicy) int {
	switch p {
	case PolicySandboxOnly:
		return 1
	case PolicyAlwaysAsk:
		return 2
	case PolicyAutoSafe:
		return 3
	case PolicyAutoVerified:
		return 4
	case PolicyAutoAll:
		return 5
	}
	return 0
}

// ApplyPolicySettable reports whether p names a tier. The set is closed for
// the reason identity.Role's is: the gate compares exactly, and a near-miss
// stored in the column is a caller silently demoted to always_ask.
func ApplyPolicySettable(p string) bool {
	return applyPolicyRank(applyPolicy(p)) > 0
}

// ParseApplyPolicyFloor reads ATLANTIS_APPLY_POLICY_FLOOR. Empty means no
// floor. Anything else must name a tier — a typo'd floor refuses boot rather
// than silently not clamping, the BuildPolicy precedent.
func ParseApplyPolicyFloor() (string, error) {
	raw := os.Getenv("ATLANTIS_APPLY_POLICY_FLOOR")
	if raw == "" {
		return "", nil
	}
	if !ApplyPolicySettable(raw) {
		return "", fmt.Errorf("ATLANTIS_APPLY_POLICY_FLOOR=%q names no apply-policy tier "+
			"(sandbox_only, always_ask, auto_safe, auto_verified, auto_all)", raw)
	}
	return raw, nil
}

// effectiveApplyPolicy resolves the caller's stored value against the floor.
//
// ” is the server default. An unknown non-empty value is treated as
// always_ask and reported — the row was written by something this build does
// not know, and always_ask keeps every apply decidable by a human while it is
// sorted out.
func (s *Service) effectiveApplyPolicy(stored string) applyPolicy {
	tier := applyPolicy(stored)
	switch {
	case stored == "":
		tier = PolicyAutoSafe
	case !ApplyPolicySettable(stored):
		fmt.Fprintf(os.Stderr, "admin: caller_identities.apply_policy %q names no tier; "+
			"treating it as always_ask\n", stored)
		tier = PolicyAlwaysAsk
	}
	if floor := applyPolicy(s.applyPolicyFloor); floor != "" &&
		applyPolicyRank(floor) < applyPolicyRank(tier) {
		tier = floor
	}
	return tier
}

// loadApplyPolicy reads one caller's tier through q, which is the apply
// transaction at gate time — deliberately uncached, unlike capability grants:
// a demotion must bind on the next apply, not five seconds later.
//
// A missing row resolves to the server default. Cert binding (0032) refuses
// an unregistered caller before any RPC body runs, so no row here means a
// deployment running without it — the tier is a narrowing of an authorized
// caller, and no row is no narrowing. Any other failure queues everything
// rather than applying it.
func (s *Service) loadApplyPolicy(ctx context.Context, q queryRower, caller string) (applyPolicy, error) {
	var stored string
	err := q.QueryRow(ctx,
		`SELECT apply_policy FROM atlantis.caller_identities WHERE caller = $1`,
		caller).Scan(&stored)
	if err == pgx.ErrNoRows {
		return s.effectiveApplyPolicy(""), nil
	}
	if err != nil {
		return "", status.Errorf(codes.FailedPrecondition,
			"admin: the apply policy for %q could not be read, so nothing may apply unattended: %v",
			caller, err)
	}
	return s.effectiveApplyPolicy(stored), nil
}

// tierRequiresApproval reports whether this tier queues this class for a
// human, given the rehearsal verdict in hand.
//
// unverified — which includes "no rehearsal exists" — never satisfies a
// verdict condition, at any tier.
func tierRequiresApproval(tier applyPolicy, class adminpb.PlanClass, verdict string) bool {
	auto := verdictAllowsAuto(verdict)
	switch tier {
	case PolicyAutoSafe:
		return class != adminpb.PlanClass_PLAN_CLASS_ADDITIVE
	case PolicyAutoVerified:
		switch class {
		case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
			return false
		case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
			return !auto
		}
		return true
	case PolicyAutoAll:
		switch class {
		case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
			return false
		case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
			return true
		}
		return !auto
	}
	// always_ask, and any value the switch does not know.
	return true
}

// verdictAllowsAuto reports whether a rehearsal verdict satisfies an auto
// tier's condition.
func verdictAllowsAuto(verdict string) bool {
	return verdict == "pass" || verdict == "pass_with_warnings"
}

// queryRower is the one method the policy read needs, satisfied by both a
// transaction and a pool.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// GetApplyPolicy answers one caller's tier: the stored value, the resolved
// effective value, and the deployment floor.
func (s *Service) GetApplyPolicy(ctx context.Context, req *adminpb.GetApplyPolicyRequest) (*adminpb.GetApplyPolicyResponse, error) {
	if req.GetCaller() == "" {
		return nil, status.Error(codes.InvalidArgument, "admin: caller is required")
	}
	var stored string
	err := s.pool.QueryRow(ctx,
		`SELECT apply_policy FROM atlantis.caller_identities WHERE caller = $1`,
		req.GetCaller()).Scan(&stored)
	if err == pgx.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "admin: caller %q is not registered", req.GetCaller())
	}
	if err != nil {
		return nil, fmt.Errorf("read apply policy for %s: %w", req.GetCaller(), err)
	}
	var rehearse bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM atlantis.caller_capabilities
WHERE caller = $1 AND capability = 'CAPABILITY_SCHEMA_REHEARSE')`,
		req.GetCaller()).Scan(&rehearse); err != nil {
		return nil, err
	}
	return &adminpb.GetApplyPolicyResponse{
		Caller:               req.GetCaller(),
		ApplyPolicy:          stored,
		EffectiveApplyPolicy: string(s.effectiveApplyPolicy(stored)),
		Floor:                s.applyPolicyFloor,
		RehearsalEnabled:     rehearse,
	}, nil
}

// SetApplyPolicy writes one caller's tier and records the change in
// atlantis.policy_events, in one transaction.
func (s *Service) SetApplyPolicy(ctx context.Context, req *adminpb.SetApplyPolicyRequest) (*adminpb.SetApplyPolicyResponse, error) {
	if err := s.requireMutablePlane("apply policy"); err != nil {
		return nil, err
	}
	if req.GetCaller() == "" {
		return nil, status.Error(codes.InvalidArgument, "admin: caller is required")
	}
	if p := req.GetApplyPolicy(); p != "" && !ApplyPolicySettable(p) {
		return nil, status.Errorf(codes.InvalidArgument,
			"admin: %q names no apply-policy tier (sandbox_only, always_ask, auto_safe, "+
				"auto_verified, auto_all; empty resets to the server default)", p)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var previous string
	err = tx.QueryRow(ctx, `
UPDATE atlantis.caller_identities ci SET apply_policy = $2
FROM (SELECT apply_policy AS prev FROM atlantis.caller_identities WHERE caller = $1 FOR UPDATE) old
WHERE ci.caller = $1
RETURNING old.prev`,
		req.GetCaller(), req.GetApplyPolicy()).Scan(&previous)
	if err == pgx.ErrNoRows {
		return nil, status.Errorf(codes.NotFound, "admin: caller %q is not registered", req.GetCaller())
	}
	if err != nil {
		return nil, fmt.Errorf("set apply policy for %s: %w", req.GetCaller(), err)
	}

	// The flag is a capability translation, like RegisterCaller's can_mutate:
	// the grant row is the state, outside every managed bundle so
	// re-registration leaves it alone. Absent leaves the grant alone too.
	rehearseGranted := false
	if req.RehearsalEnabled != nil {
		if req.GetRehearsalEnabled() {
			if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ($1, 'CAPABILITY_SCHEMA_REHEARSE', $2)
ON CONFLICT DO NOTHING`, req.GetCaller(), "apply-policy:"+req.GetUpdatedBy()); err != nil {
				return nil, fmt.Errorf("grant rehearse: %w", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `
DELETE FROM atlantis.caller_capabilities
WHERE caller = $1 AND capability = 'CAPABILITY_SCHEMA_REHEARSE'`, req.GetCaller()); err != nil {
				return nil, fmt.Errorf("revoke rehearse: %w", err)
			}
		}
	}
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM atlantis.caller_capabilities
WHERE caller = $1 AND capability = 'CAPABILITY_SCHEMA_REHEARSE')`,
		req.GetCaller()).Scan(&rehearseGranted); err != nil {
		return nil, err
	}

	payload, err := json.Marshal(map[string]any{
		"caller":            req.GetCaller(),
		"previous":          previous,
		"set":               req.GetApplyPolicy(),
		"rehearsal_enabled": rehearseGranted,
	})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.policy_events (kind, payload, actor, actor_email)
VALUES ('apply_policy_set', $1, $2, $3)`,
		payload, req.GetUpdatedBy(), req.GetUpdatedByEmail()); err != nil {
		return nil, fmt.Errorf("record policy event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.SetApplyPolicyResponse{
		Caller:               req.GetCaller(),
		ApplyPolicy:          req.GetApplyPolicy(),
		EffectiveApplyPolicy: string(s.effectiveApplyPolicy(req.GetApplyPolicy())),
		RehearsalEnabled:     rehearseGranted,
	}, nil
}
