package console

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// orgFacts is what one sweep observed about one organisation.
//
// The pointer fields are the measured ones. Nil means this sweep did not
// measure it, which upsertOrgFacts keeps distinct from zero: an organisation
// with no dead jobs and one whose admin plane refused the call are different
// states, and only the second is worth acting on.
type orgFacts struct {
	Org         string
	CollectedAt time.Time

	Reachable       bool
	UnreachableKind string
	LastError       string

	SchemaVersion *int64
	ServerVersion string
	StartedAt     *time.Time

	DeadJobs          *int32
	DeadJobsTruncated *bool

	ParkedObjects   *int32
	ParkedOverdue   *int32
	ParkedTruncated *bool

	FreezeOpen    *bool
	FreezeEndsAt  *time.Time
	MeasuredFacts bool
}

// maxFactError bounds what one row keeps of a failure.
//
// A TLS error carries the certificate chain it rejected, which runs to
// kilobytes. The kind is what a reader acts on; the text is for the case where
// the kind is not enough.
const maxFactError = 500

// upsertOrgFacts records one sweep's observation.
//
// The organisation is written as console.current_org() rather than as an
// argument: the bind is what the RESTRICTIVE policy checks, so a row can only
// be written for the organisation the transaction is bound to.
//
// COALESCE on every measured column, so a sweep that could not reach the
// organisation keeps the last numbers rather than replacing them with NULL.
// facts_at moves only when something was measured, which is what makes the two
// timestamps say how stale the numbers are.
func (o *orgStore) upsertOrgFacts(ctx context.Context, f orgFacts) error {
	if len(f.LastError) > maxFactError {
		f.LastError = f.LastError[:maxFactError]
	}
	var kind *string
	if f.UnreachableKind != "" {
		kind = &f.UnreachableKind
	}
	var factsAt *time.Time
	if f.MeasuredFacts {
		factsAt = &f.CollectedAt
	}
	var serverVersion *string
	if f.ServerVersion != "" {
		serverVersion = &f.ServerVersion
	}

	return o.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO console.org_facts (
			    org, collected_at, facts_at, reachable, unreachable_kind, last_error,
			    schema_version, server_version, started_at,
			    dead_jobs, dead_jobs_truncated,
			    parked_objects, parked_overdue, parked_truncated,
			    freeze_open, freeze_ends_at)
			VALUES (console.current_org(), $1, $2, $3, $4, $5,
			        $6, COALESCE($7, ''), $8,
			        $9, $10,
			        $11, $12, $13,
			        $14, $15)
			ON CONFLICT (org) DO UPDATE SET
			    collected_at        = EXCLUDED.collected_at,
			    facts_at            = COALESCE(EXCLUDED.facts_at, console.org_facts.facts_at),
			    reachable           = EXCLUDED.reachable,
			    unreachable_kind    = EXCLUDED.unreachable_kind,
			    last_error          = EXCLUDED.last_error,
			    schema_version      = COALESCE(EXCLUDED.schema_version, console.org_facts.schema_version),
			    server_version      = COALESCE(NULLIF(EXCLUDED.server_version, ''), console.org_facts.server_version),
			    started_at          = COALESCE(EXCLUDED.started_at, console.org_facts.started_at),
			    dead_jobs           = COALESCE(EXCLUDED.dead_jobs, console.org_facts.dead_jobs),
			    dead_jobs_truncated = COALESCE(EXCLUDED.dead_jobs_truncated, console.org_facts.dead_jobs_truncated),
			    parked_objects      = COALESCE(EXCLUDED.parked_objects, console.org_facts.parked_objects),
			    parked_overdue      = COALESCE(EXCLUDED.parked_overdue, console.org_facts.parked_overdue),
			    parked_truncated    = COALESCE(EXCLUDED.parked_truncated, console.org_facts.parked_truncated),
			    freeze_open         = COALESCE(EXCLUDED.freeze_open, console.org_facts.freeze_open),
			    freeze_ends_at      = CASE WHEN EXCLUDED.freeze_open IS NULL
			                               THEN console.org_facts.freeze_ends_at
			                               ELSE EXCLUDED.freeze_ends_at END
		`,
			f.CollectedAt, factsAt, f.Reachable, kind, f.LastError,
			f.SchemaVersion, serverVersion, f.StartedAt,
			f.DeadJobs, f.DeadJobsTruncated,
			f.ParkedObjects, f.ParkedOverdue, f.ParkedTruncated,
			f.FreezeOpen, f.FreezeEndsAt)
		return err
	})
}

// orgFacts returns what the last sweep recorded, and whether there is a row.
//
// Bound, so it reads one organisation's row and could not read another's. The
// fleet view is the gauges in metrics.go; nothing reads this table across
// organisations.
func (o *orgStore) orgFacts(ctx context.Context) (orgFacts, bool, error) {
	var (
		f     orgFacts
		found bool
	)
	err := o.tx(ctx, func(tx pgx.Tx) error {
		var (
			factsAt  *time.Time
			kind     *string
			endsAt   *time.Time
			started  *time.Time
			scanning orgFacts
		)
		scanErr := tx.QueryRow(ctx, `
			SELECT org, collected_at, facts_at, reachable, unreachable_kind, last_error,
			       schema_version, server_version, started_at,
			       dead_jobs, dead_jobs_truncated,
			       parked_objects, parked_overdue, parked_truncated,
			       freeze_open, freeze_ends_at
			  FROM console.org_facts
		`).Scan(&scanning.Org, &scanning.CollectedAt, &factsAt, &scanning.Reachable, &kind,
			&scanning.LastError,
			&scanning.SchemaVersion, &scanning.ServerVersion, &started,
			&scanning.DeadJobs, &scanning.DeadJobsTruncated,
			&scanning.ParkedObjects, &scanning.ParkedOverdue, &scanning.ParkedTruncated,
			&scanning.FreezeOpen, &endsAt)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		if kind != nil {
			scanning.UnreachableKind = *kind
		}
		scanning.MeasuredFacts = factsAt != nil
		scanning.StartedAt = started
		scanning.FreezeEndsAt = endsAt
		f, found = scanning, true
		return nil
	})
	return f, found, err
}
