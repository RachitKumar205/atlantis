package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Asking for an organisation's console credentials to be replaced.
//
// Cloud cannot do the work: the certificates live in a Secret in the
// organisation's namespace and only the provisioner holds Kubernetes
// credentials. So the request is a mark on the row and the provisioner acts on
// it, the same shape `cloud org purge` uses for destruction.

// RequestConsoleRotation marks one organisation for a console credential
// rotation on the provisioner's next pass.
//
// Idempotent in effect but not in timestamp: asking twice moves the mark
// forward, which is what ClearConsoleRotationRequest compares against.
func (s *Store) RequestConsoleRotation(ctx context.Context, org string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET console_rotate_requested_at = NOW(),
		       updated_at                  = NOW()
		 WHERE org = $1
	`, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// ConsoleRotationRequests returns every organisation with an outstanding
// request, and when it was made.
//
// The timestamp is returned because ClearConsoleRotationRequest compares
// against it.
//
// One query for the whole fleet, for a column that is almost always NULL.
func (s *Store) ConsoleRotationRequests(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT org, console_rotate_requested_at
		  FROM cloud.org_provisioning
		 WHERE console_rotate_requested_at IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]time.Time{}
	for rows.Next() {
		var org string
		var at time.Time
		if err := rows.Scan(&org, &at); err != nil {
			return nil, err
		}
		out[org] = at
	}
	return out, rows.Err()
}

// ClearConsoleRotationRequest marks a request satisfied, but only the one that
// was seen.
//
// The clear is conditional on requested_at matching seen, the timestamp the
// pass observed. An unconditional SET NULL would discard a request made while
// that pass was still running, leaving the credential it was asked twice to
// replace still in use. A later request survives and is honoured on the next
// pass.
func (s *Store) ClearConsoleRotationRequest(ctx context.Context, org string, seen time.Time) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET console_rotate_requested_at = NULL,
		       updated_at                  = NOW()
		 WHERE org = $1
		   AND console_rotate_requested_at <= $2
	`, org, seen)
	// No RowsAffected check: matching nothing is correct when a newer request
	// has replaced the one this pass handled.
	return err
}
