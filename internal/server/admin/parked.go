package admin

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// maxParkedLimit clamps one request.
//
// The register has two halves with very different growth: live registrations
// are bounded by how much somebody has removed and not yet restored, while
// reaped rows are retained indefinitely as the audit trail. Without a ceiling,
// a caller asking for 2^31-1 rows makes the server materialise and serialize
// the entire history — measured at ~146 MB of heap for 300k rows, on a
// capability every registered caller holds. GetSchemaHistory clamps at 100 for
// the same reason.
const maxParkedLimit = 500

const defaultParkedLimit = 100

// ListParkedObjects reads the register of objects a destructive migration
// parked instead of dropping.
//
// This exists because the register was reachable only by hand-written SQL
// against atlantis.parked_objects — a table no RPC, CLI, console page or doc
// mentioned. An operator who removed an entity by mistake had no way to learn
// that the rows were still there, or how long they had to change their mind,
// which made a recoverable delete indistinguishable from an unrecoverable one.
//
// Read-only on purpose. Extending a retention window or reaping early are
// deliberate acts with data consequences, and they stay documented UPDATEs
// rather than buttons: the value of the 30-day window comes from it being hard
// to shorten by accident.
//
// The two halves are queried separately rather than with one OR'd predicate.
// `WHERE ($1 OR reaped_at IS NULL)` collapses to no predicate when including
// reaped rows, which makes the partial index unusable and turns the read into
// a parallel sequential scan over the whole audit history — on the pool that
// also serves auth and the admin plane. Split, each half uses its own index,
// and each gets its own budget so that a long list of live objects cannot
// crowd out the audit rows the caller explicitly asked for.
func (s *Service) ListParkedObjects(ctx context.Context, req *adminpb.ListParkedObjectsRequest) (*adminpb.ListParkedObjectsResponse, error) {
	// No-PG path, matching the rest of the package: an empty probe request
	// reaches the pool with nothing to reject on first.
	if s.pool == nil {
		return &adminpb.ListParkedObjectsResponse{}, nil
	}

	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultParkedLimit
	}
	if limit > maxParkedLimit {
		limit = maxParkedLimit
	}

	const cols = `id, kind, schema_name, object_name, coalesce(parent_table, ''),
	              original_schema, original_name, parked_at, reap_after, reaped_at,
	              attempts, coalesce(last_error, ''), next_attempt_after`

	// Live registrations first, soonest to expire first: the question this
	// answers is "what am I about to lose", so the ordering is part of the
	// answer. Uses parked_objects_due_idx.
	live, liveMore, err := s.queryParked(ctx, `
SELECT `+cols+`
  FROM atlantis.parked_objects
 WHERE reaped_at IS NULL
 ORDER BY reap_after
 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	out := live
	hasMore := liveMore
	if req.GetIncludeReaped() {
		// Most recently reaped first — "what happened lately" rather than
		// "what happened first". Uses parked_objects_reaped_idx.
		reaped, reapedMore, err := s.queryParked(ctx, `
SELECT `+cols+`
  FROM atlantis.parked_objects
 WHERE reaped_at IS NOT NULL
 ORDER BY reaped_at DESC
 LIMIT $1`, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, reaped...)
		hasMore = hasMore || reapedMore
	}

	return &adminpb.ListParkedObjectsResponse{Objects: out, HasMore: hasMore}, nil
}

// queryParked runs one half of the register and reports whether it was
// truncated.
//
// Fetches limit+1 and discards the extra, the same trick GetSchemaHistory
// uses: it distinguishes "this is everything" from "the page stops here"
// without a second COUNT over a table that grows forever.
func (s *Service) queryParked(ctx context.Context, q string, limit int) ([]*adminpb.ParkedObject, bool, error) {
	rs, err := s.pool.Query(ctx, q, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rs.Close()

	out := make([]*adminpb.ParkedObject, 0, limit)
	for rs.Next() {
		o, err := scanParkedRow(rs)
		if err != nil {
			return nil, false, err
		}
		out = append(out, o)
	}
	if err := rs.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

func scanParkedRow(rs pgx.Rows) (*adminpb.ParkedObject, error) {
	var (
		o                adminpb.ParkedObject
		parkedAt         time.Time
		reapAfter        time.Time
		reapedAt         *time.Time
		nextAttemptAfter *time.Time
		attempts         int32
	)
	if err := rs.Scan(&o.Id, &o.Kind, &o.SchemaName, &o.ObjectName, &o.ParentTable,
		&o.OriginalSchema, &o.OriginalName, &parkedAt, &reapAfter, &reapedAt,
		&attempts, &o.LastError, &nextAttemptAfter); err != nil {
		return nil, err
	}
	o.Attempts = attempts
	o.ParkedAt = parkedAt.UTC().Format(time.RFC3339)
	o.ReapAfter = reapAfter.UTC().Format(time.RFC3339)
	if reapedAt != nil {
		o.ReapedAt = reapedAt.UTC().Format(time.RFC3339)
	}
	if nextAttemptAfter != nil {
		o.NextAttemptAfter = nextAttemptAfter.UTC().Format(time.RFC3339)
	}
	return &o, nil
}
