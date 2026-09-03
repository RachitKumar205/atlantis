package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// Protected entities and freeze windows: two org-wide gates the apply
// transaction consults beside the change policy and the caller's tier.

// protectedRule is one approval floor keyed by pattern.
type protectedRule struct {
	Pattern string
	Floor   string // require_approval | admin_only; anything else enforced as admin_only
	Reason  string
}

// AdminOnly reports whether this rule's floor demands an admin decision. An
// unrecognised floor does — the row was written by something this build does
// not know, and the wider requirement is the one that cannot be regretted.
func (r protectedRule) AdminOnly() bool { return r.Floor != "require_approval" }

func loadProtectedEntities(ctx context.Context, q pgxQuerier) ([]protectedRule, error) {
	rows, err := q.Query(ctx,
		`SELECT pattern, floor, reason FROM atlantis.protected_entities ORDER BY pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protectedRule
	for rows.Next() {
		var r protectedRule
		if err := rows.Scan(&r.Pattern, &r.Floor, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchProtected reports the first rule any of ids matches. Patterns are
// exact `namespace.Entity` or `namespace.*`.
func matchProtected(rules []protectedRule, ids []string) *protectedRule {
	for i := range rules {
		r := &rules[i]
		if ns, ok := strings.CutSuffix(r.Pattern, ".*"); ok {
			for _, id := range ids {
				if strings.HasPrefix(id, ns+".") {
					return r
				}
			}
			continue
		}
		for _, id := range ids {
			if id == r.Pattern {
				return r
			}
		}
	}
	return nil
}

// diffEntityIDs collects the deduplicated entity ids the live diff touches.
// From the *codegen.Diff the gate already holds, never from the stored JSON —
// planEntityIDs returns nil on a row it cannot decode, which is fine for a
// display strip and fatal for a gate.
func diffEntityIDs(d *codegen.Diff) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range d.All() {
		if c.EntityID == "" || seen[c.EntityID] {
			continue
		}
		seen[c.EntityID] = true
		out = append(out, c.EntityID)
	}
	return out
}

// protectedHitFor answers whether this diff touches a protected entity.
//
// Fail closed: with rules configured, a non-empty diff that names no entities
// is treated as touching one at the admin_only floor — a diff the matcher
// cannot see is not a diff the protection can be waived for.
func (s *Service) protectedHitFor(ctx context.Context, q pgxQuerier, d *codegen.Diff) (*protectedRule, error) {
	rules, err := loadProtectedEntities(ctx, q)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"admin: the protected entities could not be read, so no change may apply unattended: %v", err)
	}
	if len(rules) == 0 {
		return nil, nil
	}
	ids := diffEntityIDs(d)
	if d.Len() > 0 && len(ids) == 0 {
		return &protectedRule{Pattern: "(unattributable diff)", Floor: "admin_only",
			Reason: "the diff names no entities, so protection cannot be matched"}, nil
	}
	return matchProtected(rules, ids), nil
}

// freezeHit is the freeze that stops an apply: the latest end among the
// windows covering it, so the refusal names when applying resumes.
type freezeHit struct {
	EndsAt time.Time
	Reason string
}

// activeFreezeFor answers whether a freeze window covers now for any class
// present. A window with no classes freezes every class.
func (s *Service) activeFreezeFor(ctx context.Context, q pgxQuerier, now time.Time, present []codegen.ChangeClass) (*freezeHit, error) {
	if len(present) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
SELECT ends_at, classes, reason FROM atlantis.freeze_windows
 WHERE starts_at <= $1 AND ends_at > $1`, now)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"admin: the freeze windows could not be read, so no change may apply: %v", err)
	}
	defer rows.Close()

	names := make(map[string]bool, len(present))
	for _, c := range present {
		names[c.String()] = true
	}
	var hit *freezeHit
	for rows.Next() {
		var endsAt time.Time
		var classes []string
		var reason string
		if err := rows.Scan(&endsAt, &classes, &reason); err != nil {
			return nil, err
		}
		matches := len(classes) == 0
		for _, c := range classes {
			if names[c] {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if hit == nil || endsAt.After(hit.EndsAt) {
			hit = &freezeHit{EndsAt: endsAt, Reason: reason}
		}
	}
	return hit, rows.Err()
}

// validateProtectedPattern normalizes and checks a pattern: an exact
// `namespace.Entity`, or `namespace.*` for the whole namespace.
func validateProtectedPattern(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	i := strings.LastIndexByte(p, '.')
	if i <= 0 || i == len(p)-1 {
		return "", fmt.Errorf("pattern %q is not `namespace.Entity` or `namespace.*`", raw)
	}
	ns, name := p[:i], p[i+1:]
	for _, c := range ns {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return "", fmt.Errorf("pattern %q names no namespace this server could hold", raw)
		}
	}
	if name == "*" {
		return p, nil
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return "", fmt.Errorf("pattern %q names no entity this server could hold", raw)
		}
	}
	return p, nil
}

func (s *Service) recordPolicyEvent(ctx context.Context, tx pgx.Tx, kind string, payload any, actor, email string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.policy_events (kind, payload, actor, actor_email)
VALUES ($1, $2, $3, $4)`, kind, raw, actor, email); err != nil {
		return fmt.Errorf("record policy event %s: %w", kind, err)
	}
	return nil
}

func (s *Service) ListProtectedEntities(ctx context.Context, _ *adminpb.ListProtectedEntitiesRequest) (*adminpb.ListProtectedEntitiesResponse, error) {
	if s.pool == nil {
		return &adminpb.ListProtectedEntitiesResponse{}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT pattern, floor, reason, created_by, created_at
FROM atlantis.protected_entities ORDER BY pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &adminpb.ListProtectedEntitiesResponse{}
	for rows.Next() {
		var e adminpb.ProtectedEntity
		var createdAt time.Time
		if err := rows.Scan(&e.Pattern, &e.Floor, &e.Reason, &e.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		e.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out.Entities = append(out.Entities, &e)
	}
	return out, rows.Err()
}

func (s *Service) PutProtectedEntity(ctx context.Context, req *adminpb.PutProtectedEntityRequest) (*adminpb.PutProtectedEntityResponse, error) {
	if err := s.requireMutablePlane("protected entities"); err != nil {
		return nil, err
	}
	e := req.GetEntity()
	pattern, err := validateProtectedPattern(e.GetPattern())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "admin: "+err.Error())
	}
	if f := e.GetFloor(); f != "require_approval" && f != "admin_only" {
		return nil, status.Errorf(codes.InvalidArgument,
			"admin: floor %q is not require_approval or admin_only", f)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.protected_entities (pattern, floor, reason, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (pattern) DO UPDATE SET floor = EXCLUDED.floor, reason = EXCLUDED.reason`,
		pattern, e.GetFloor(), e.GetReason(), req.GetUpdatedBy()); err != nil {
		return nil, fmt.Errorf("put protected entity: %w", err)
	}
	if err := s.recordPolicyEvent(ctx, tx, "protected_entity_put", map[string]string{
		"pattern": pattern, "floor": e.GetFloor(), "reason": e.GetReason(),
	}, req.GetUpdatedBy(), req.GetUpdatedByEmail()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.PutProtectedEntityResponse{Entity: &adminpb.ProtectedEntity{
		Pattern: pattern, Floor: e.GetFloor(), Reason: e.GetReason(), CreatedBy: req.GetUpdatedBy(),
	}}, nil
}

func (s *Service) DeleteProtectedEntity(ctx context.Context, req *adminpb.DeleteProtectedEntityRequest) (*adminpb.DeleteProtectedEntityResponse, error) {
	if err := s.requireMutablePlane("protected entities"); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx,
		`DELETE FROM atlantis.protected_entities WHERE pattern = $1`, req.GetPattern())
	if err != nil {
		return nil, fmt.Errorf("delete protected entity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, status.Errorf(codes.NotFound, "admin: no protection for %q", req.GetPattern())
	}
	if err := s.recordPolicyEvent(ctx, tx, "protected_entity_deleted", map[string]string{
		"pattern": req.GetPattern(),
	}, req.GetUpdatedBy(), req.GetUpdatedByEmail()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.DeleteProtectedEntityResponse{}, nil
}

func (s *Service) ListFreezeWindows(ctx context.Context, _ *adminpb.ListFreezeWindowsRequest) (*adminpb.ListFreezeWindowsResponse, error) {
	if s.pool == nil {
		return &adminpb.ListFreezeWindowsResponse{}, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, starts_at, ends_at, display_tz, classes, reason, created_by, created_at
FROM atlantis.freeze_windows ORDER BY starts_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &adminpb.ListFreezeWindowsResponse{}
	for rows.Next() {
		var w adminpb.FreezeWindow
		var startsAt, endsAt, createdAt time.Time
		if err := rows.Scan(&w.Id, &startsAt, &endsAt, &w.DisplayTz, &w.Classes,
			&w.Reason, &w.CreatedBy, &createdAt); err != nil {
			return nil, err
		}
		w.StartsAt = startsAt.UTC().Format(time.RFC3339)
		w.EndsAt = endsAt.UTC().Format(time.RFC3339)
		w.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		out.Windows = append(out.Windows, &w)
	}
	return out, rows.Err()
}

// freezeClassNames is the closed set a window may freeze, spelled the way
// ChangeClass.String does — the spelling the gate compares against.
func freezeClassNames() map[string]bool {
	out := map[string]bool{}
	for _, c := range ChangeClasses() {
		out[c.String()] = true
	}
	return out
}

func (s *Service) CreateFreezeWindow(ctx context.Context, req *adminpb.CreateFreezeWindowRequest) (*adminpb.CreateFreezeWindowResponse, error) {
	if err := s.requireMutablePlane("freeze windows"); err != nil {
		return nil, err
	}
	w := req.GetWindow()
	starts, err := time.Parse(time.RFC3339, w.GetStartsAt())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "admin: starts_at: %v", err)
	}
	ends, err := time.Parse(time.RFC3339, w.GetEndsAt())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "admin: ends_at: %v", err)
	}
	if !ends.After(starts) {
		return nil, status.Error(codes.InvalidArgument, "admin: the window ends before it starts")
	}
	known := freezeClassNames()
	for _, c := range w.GetClasses() {
		if !known[c] {
			return nil, status.Errorf(codes.InvalidArgument,
				"admin: %q names no plan class a window can freeze", c)
		}
	}
	// A nil slice inserts NULL past the column default; an empty window
	// freezes every class and is stored as the empty array.
	classes := w.GetClasses()
	if classes == nil {
		classes = []string{}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var id int64
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
INSERT INTO atlantis.freeze_windows (starts_at, ends_at, display_tz, classes, reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, created_at`,
		starts, ends, w.GetDisplayTz(), classes, w.GetReason(), req.GetUpdatedBy(),
	).Scan(&id, &createdAt); err != nil {
		return nil, fmt.Errorf("create freeze window: %w", err)
	}
	if err := s.recordPolicyEvent(ctx, tx, "freeze_window_created", map[string]any{
		"id": id, "starts_at": starts.UTC().Format(time.RFC3339),
		"ends_at": ends.UTC().Format(time.RFC3339), "classes": w.GetClasses(),
		"reason": w.GetReason(),
	}, req.GetUpdatedBy(), req.GetUpdatedByEmail()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.CreateFreezeWindowResponse{Window: &adminpb.FreezeWindow{
		Id: id, StartsAt: starts.UTC().Format(time.RFC3339), EndsAt: ends.UTC().Format(time.RFC3339),
		DisplayTz: w.GetDisplayTz(), Classes: w.GetClasses(), Reason: w.GetReason(),
		CreatedBy: req.GetUpdatedBy(), CreatedAt: createdAt.UTC().Format(time.RFC3339),
	}}, nil
}

func (s *Service) DeleteFreezeWindow(ctx context.Context, req *adminpb.DeleteFreezeWindowRequest) (*adminpb.DeleteFreezeWindowResponse, error) {
	if err := s.requireMutablePlane("freeze windows"); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	tag, err := tx.Exec(ctx, `DELETE FROM atlantis.freeze_windows WHERE id = $1`, req.GetId())
	if err != nil {
		return nil, fmt.Errorf("delete freeze window: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, status.Errorf(codes.NotFound, "admin: no freeze window %d", req.GetId())
	}
	if err := s.recordPolicyEvent(ctx, tx, "freeze_window_deleted", map[string]any{
		"id": req.GetId(),
	}, req.GetUpdatedBy(), req.GetUpdatedByEmail()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &adminpb.DeleteFreezeWindowResponse{}, nil
}
