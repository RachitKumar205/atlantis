package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Asking for an organisation's images to be replaced.
//
// Cloud cannot do the work: the workloads live in the organisation's namespace
// and only the provisioner holds Kubernetes credentials. So the request is a
// mark on the row and the provisioner acts on it, the same shape
// RequestConsoleRotation uses.

// ImageRoll is one outstanding request.
//
// Kinds is carried because the three images differ in cost — rolling postgres
// restarts a single-instance database — and RequestedAt because
// ClearImageRollRequest compares against it.
//
// Attempts is how many passes have tried this request and failed. The
// provisioner orders on it before RequestedAt, so a request that cannot succeed
// stops holding the queue.
type ImageRoll struct {
	Kinds       []string
	RequestedAt time.Time
	Attempts    int
}

// RequestImageRoll marks one organisation for an image roll on the
// provisioner's next pass.
//
// Idempotent in effect but not in timestamp: asking twice moves the mark
// forward, which is what ClearImageRollRequest compares against. A second
// request replaces the kinds rather than adding to them, so what is stored is
// always the last thing asked for.
func (s *Store) RequestImageRoll(ctx context.Context, org string, kinds []string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	if len(kinds) == 0 {
		return errors.New("at least one image kind is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_requested_at = NOW(),
		       image_roll_kinds        = $2,
		       image_roll_attempts     = 0,
		       updated_at              = NOW()
		 WHERE org = $1
	`, org, kinds)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// CancelImageRoll withdraws an outstanding request.
//
// The per-pass cap exists so a fleet-wide roll can be stopped after watching
// the first organisation. Without this the only way to stop one is to scale the
// provisioner to zero.
//
// Reports whether a request was outstanding, so a command can tell an operator
// it cancelled nothing.
func (s *Store) CancelImageRoll(ctx context.Context, org string) (bool, error) {
	if org == "" {
		return false, errors.New("an organisation is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_requested_at = NULL,
		       image_roll_kinds        = NULL,
		       image_roll_attempts     = 0,
		       updated_at              = NOW()
		 WHERE org = $1
		   AND image_roll_requested_at IS NOT NULL
	`, org)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RecordImageRollFailure counts one failed attempt against a request.
//
// The request stays outstanding — the next pass retries it — but the count
// moves it behind every request that has not been tried. One organisation whose
// roll can never succeed would otherwise hold the only slot on every pass and
// no other organisation would ever roll.
func (s *Store) RecordImageRollFailure(ctx context.Context, org string, seen time.Time) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_attempts = image_roll_attempts + 1,
		       updated_at          = NOW()
		 WHERE org = $1
		   AND image_roll_requested_at <= $2
	`, org, seen)
	// Bounded by the observed timestamp for the reason the clear is: a request
	// made while the roll was running is a fresh one, and must not inherit the
	// failure count of the one it replaced.
	return err
}

// ImageRollRequests returns every organisation with an outstanding request.
//
// One query for the whole fleet, for a column that is almost always NULL.
func (s *Store) ImageRollRequests(ctx context.Context) (map[string]ImageRoll, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT org, image_roll_kinds, image_roll_requested_at, image_roll_attempts
		  FROM cloud.org_provisioning
		 WHERE image_roll_requested_at IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]ImageRoll{}
	for rows.Next() {
		var org string
		var r ImageRoll
		if err := rows.Scan(&org, &r.Kinds, &r.RequestedAt, &r.Attempts); err != nil {
			return nil, err
		}
		out[org] = r
	}
	return out, rows.Err()
}

// ClearImageRollRequest marks a request satisfied, but only the one that was
// seen.
//
// Conditional on requested_at matching seen, the timestamp the pass observed.
// An unconditional clear would discard a request made while the roll was
// running — which is how an operator asks for postgres after watching server
// land — leaving the second request unhonoured. A later request survives and is
// acted on next pass.
func (s *Store) ClearImageRollRequest(ctx context.Context, org string, seen time.Time) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_requested_at = NULL,
		       image_roll_kinds        = NULL,
		       image_roll_attempts     = 0,
		       updated_at              = NOW()
		 WHERE org = $1
		   AND image_roll_requested_at <= $2
	`, org, seen)
	// No RowsAffected check: matching nothing is correct when a newer request
	// has replaced the one this pass handled.
	return err
}
