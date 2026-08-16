package admin

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// The change policy: which classes of schema change may apply without a human.
//
// This is ENFORCED. gateOnChangePolicy reads this table and refuses the apply,
// recording a plan for a reviewer to decide. Two call sites, and both are
// inside the transaction that would otherwise run the DDL:
//
//	admin.go     ApplyMigration, after emit and before the drift checks
//	backfill.go  BeginBackfillPlan, which reaches the same DDL by another door
//
// The table, the RPCs and the console panel shipped one release ahead of that,
// deliberately, so an operator could see the rule they were about to be held to
// and agree it before it decided anything. A gate that appears in the same
// release that starts refusing applies is a gate people learn about from an
// outage. That release has passed; do not reintroduce the caveat.
//
// Nothing here caches, and that is the reason the table lives in atlantis.*
// rather than in config. authz.postgresGrants caches for five seconds because
// it sits on every RPC's hot path; apply does not, and a cached gate decision
// is a window in which a just-tightened rule still permits.

// ChangeClasses are the classes a rule can exist for.
//
// Derived from codegen.Diff's buckets rather than from the PlanClass enum,
// because the buckets are what a diff is actually sorted into. The enum
// carries two more values and neither can reach a policy decision: UNPARSEABLE
// means the DSL did not compile, so the apply is refused long before any rule
// is consulted, and UNSPECIFIED is the zero value.
//
// TestEveryChangeClassHasAPolicyKey walks the buckets reflectively and fails
// when one has no entry here, so a fifth bucket cannot quietly arrive with no
// rule attached to it.
func ChangeClasses() []codegen.ChangeClass {
	return []codegen.ChangeClass{
		codegen.ClassAdditive,
		codegen.ClassBackfillRequired,
		codegen.ClassCrossCallerBreaking,
		codegen.ClassDestructive,
	}
}

// ChangePolicy is one class's rule.
type ChangePolicy struct {
	Class           adminpb.PlanClass
	RequireApproval bool
	ApproverRole    string
	UpdatedAt       time.Time
	UpdatedBy       string
}

// DefaultApproverRole is what a rule gets when nobody has named one. It is a
// console role name, and 'admin' is the console's own highest role.
const DefaultApproverRole = "admin"

// effectiveChangePolicy resolves what applies to one class, given whatever the
// table holds.
//
// A class with no row requires approval. So does a class whose stored name
// nobody recognises, and a class outside the set a diff can produce. The three
// cases are one rule on purpose: every way of failing to find a rule fails the
// same way, and the safe way. A deployment where somebody deleted a row, or
// mistyped one, is a deployment where more applies stop for a human — never
// one where fewer do.
func effectiveChangePolicy(stored map[adminpb.PlanClass]ChangePolicy, class adminpb.PlanClass) ChangePolicy {
	if p, ok := stored[class]; ok {
		if p.ApproverRole == "" {
			p.ApproverRole = DefaultApproverRole
		}
		return p
	}
	return ChangePolicy{
		Class:           class,
		RequireApproval: true,
		ApproverRole:    DefaultApproverRole,
	}
}

// loadChangePolicy reads every stored rule.
//
// A row whose change_class does not name a PlanClass is skipped rather than
// rejected. That is what lets the table carry a class this build has never
// heard of during a rolling deploy, and it costs nothing: a class this build
// cannot name is a class it cannot produce a diff for either, and one it can
// name but has no row for already fails closed.
func loadChangePolicy(ctx context.Context, q pgxQuerier) (map[adminpb.PlanClass]ChangePolicy, []string, error) {
	rows, err := q.Query(ctx, `
SELECT change_class, require_approval, approver_role, updated_at, updated_by
FROM atlantis.change_policy`)
	if err != nil {
		return nil, nil, fmt.Errorf("load change policy: %w", err)
	}
	defer rows.Close()

	out := map[adminpb.PlanClass]ChangePolicy{}
	var unrecognized []string
	for rows.Next() {
		var name, role, by string
		var require bool
		var at time.Time
		if err := rows.Scan(&name, &require, &role, &at, &by); err != nil {
			return nil, nil, fmt.Errorf("scan change policy: %w", err)
		}
		num, ok := adminpb.PlanClass_value[name]
		if !ok {
			unrecognized = append(unrecognized, name)
			continue
		}
		class := adminpb.PlanClass(num)
		if role == "" {
			role = DefaultApproverRole
		}
		out[class] = ChangePolicy{
			Class: class, RequireApproval: require, ApproverRole: role,
			UpdatedAt: at, UpdatedBy: by,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate change policy: %w", err)
	}
	return out, unrecognized, nil
}

// pgxQuerier is the read surface loadChangePolicy needs, so it can run on the
// pool for a read RPC and on the transaction for the apply gate. The apply
// path must read inside its own transaction — that is what serialises the gate
// against a policy edit committing halfway through.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// GetChangePolicy reports the rule for every class a diff can produce.
//
// Every class, not every row: a class with no row is reported with what would
// actually happen to it, so the console shows the effective policy rather than
// a gap the reader has to know the fail-closed rule to interpret.
func (s *Service) GetChangePolicy(ctx context.Context, _ *adminpb.GetChangePolicyRequest) (*adminpb.GetChangePolicyResponse, error) {
	// No-PG test path, matching the other read methods in this package. It
	// reports every class as requiring approval, which is not a placeholder —
	// it is the same answer a database holding no rows would produce, and it
	// is the right one: with nothing to read, nothing has been permitted.
	if s.pool == nil {
		return &adminpb.GetChangePolicyResponse{Entries: changePolicyToPB(nil)}, nil
	}
	stored, unrecognized, err := loadChangePolicy(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	for _, name := range unrecognized {
		// Warned rather than returned. The rule the typo was meant to describe
		// is already failing closed, so the request is answerable; what is
		// missing is any way for the operator to find out, and silence here is
		// how a mistyped class sits unnoticed for a release.
		fmt.Fprintf(os.Stderr, "admin: change_policy row %q names no known plan class; "+
			"the class it was meant to describe requires approval\n", name)
	}
	return &adminpb.GetChangePolicyResponse{Entries: changePolicyToPB(stored)}, nil
}

// SetChangePolicy writes the rules it is given and leaves the rest alone.
func (s *Service) SetChangePolicy(ctx context.Context, req *adminpb.SetChangePolicyRequest) (*adminpb.SetChangePolicyResponse, error) {
	if err := s.requireMutablePlane("change policy"); err != nil {
		return nil, err
	}
	if len(req.GetEntries()) == 0 {
		return nil, fmt.Errorf("admin: no policy entries to set")
	}
	for _, e := range req.GetEntries() {
		if !policyClassIsSettable(e.GetChangeClass()) {
			return nil, fmt.Errorf("admin: %s is not a class a rule can apply to — "+
				"a plan is only unparseable when the DSL did not compile, which is "+
				"refused before any policy is consulted", e.GetChangeClass())
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, e := range req.GetEntries() {
		role := e.GetApproverRole()
		if role == "" {
			role = DefaultApproverRole
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.change_policy (change_class, require_approval, approver_role, updated_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (change_class) DO UPDATE SET
    require_approval = EXCLUDED.require_approval,
    approver_role    = EXCLUDED.approver_role,
    updated_at       = now(),
    updated_by       = EXCLUDED.updated_by`,
			e.GetChangeClass().String(), e.GetRequireApproval(), role, req.GetUpdatedBy(),
		); err != nil {
			return nil, fmt.Errorf("write change policy for %s: %w", e.GetChangeClass(), err)
		}
	}

	// Read back inside the same transaction so the response cannot describe a
	// state that a concurrent edit had already replaced by the time it was
	// rendered.
	stored, _, err := loadChangePolicy(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.SetChangePolicyResponse{Entries: changePolicyToPB(stored)}, nil
}

// policyClassIsSettable reports whether a rule can exist for this class.
func policyClassIsSettable(c adminpb.PlanClass) bool {
	for _, cc := range ChangeClasses() {
		if planClassToPB(translateClass(cc)) == c {
			return true
		}
	}
	return false
}

// changePolicyToPB renders every settable class in a stable order, filling in
// the effective rule for classes with no row.
func changePolicyToPB(stored map[adminpb.PlanClass]ChangePolicy) []*adminpb.ChangePolicyEntry {
	out := make([]*adminpb.ChangePolicyEntry, 0, len(ChangeClasses()))
	for _, cc := range ChangeClasses() {
		class := planClassToPB(translateClass(cc))
		p := effectiveChangePolicy(stored, class)
		e := &adminpb.ChangePolicyEntry{
			ChangeClass:     class,
			RequireApproval: p.RequireApproval,
			ApproverRole:    p.ApproverRole,
			UpdatedBy:       p.UpdatedBy,
		}
		if !p.UpdatedAt.IsZero() {
			e.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, e)
	}
	return out
}
