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
// forward, which is what makes the clear below safe. An operator who asks again
// while a rotation is already running is asking for a rotation that starts
// after their second request, and moving the timestamp is how that is recorded.
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
// The timestamp is returned rather than a bare set because it is what
// ClearConsoleRotationRequest needs in order not to discard a request that
// arrived while the rotation it is finishing was already under way.
//
// One query for the whole fleet rather than one per organisation: reconcile
// already iterates every ready organisation, and asking the database per
// organisation would turn a reconcile pass into a query per tenant for a column
// that is almost always NULL.
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
// # Why this is not simply SET NULL
//
// An operator can ask again while the provisioner is part-way through the
// rotation their first request triggered — during an incident that is the
// likely case, not an unlikely one. An unconditional clear would drop the
// second request, and the credential the operator asked twice to replace would
// be the one still in use.
//
// Comparing against the timestamp the pass observed means a request made after
// that moment survives and is honoured on the next pass.
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
	// No RowsAffected check. Matching nothing is the correct outcome when a
	// newer request has replaced the one this pass handled, and it is not
	// distinguishable here from the organisation having been deleted meanwhile.
	return err
}
