package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The provisioning queue: what decides an organisation should have an atlantis,
// and what records how that went. Migration 0005 holds the table.
//
// It follows the shape of atlantis.jobs:
//
//   - The claim is one statement. A select followed by an update leaves a
//     window in which another provisioner takes the same row; a CTE holding
//     FOR UPDATE SKIP LOCKED and feeding the update does not.
//
//   - Claimability is a lease, not a flag. A row becomes claimable again when
//     claimed_until passes, so a provisioner that dies mid-work frees its row
//     without a sweeper.

// ProvisioningState is where an organisation has got to.
type ProvisioningState string

const (
	// StatePending is queued and never yet attempted.
	StatePending ProvisioningState = "pending"
	// StateProvisioning is claimed by a provisioner right now — or by one that
	// has since died, until its lease expires.
	StateProvisioning ProvisioningState = "provisioning"
	// StateReady is serving.
	StateReady ProvisioningState = "ready"
	// StateFailed is an attempt that did not finish. Not terminal: it is
	// claimable again once next_attempt_after passes.
	StateFailed ProvisioningState = "failed"

	// StateDeleted is soft-deleted: not serving, hidden from the organisation
	// list, and restorable until purge_after passes.
	//
	// The namespace is untouched in this state. There are no database backups,
	// so this window is the only recovery path.
	StateDeleted ProvisioningState = "deleted"

	// StatePurging is claimed by a provisioner that is tearing the organisation
	// down. Beyond here nothing is recoverable.
	StatePurging ProvisioningState = "purging"
)

// ReadyOrgs and Requeue below both filter on state = 'ready', so a soft-deleted
// organisation is invisible to the reconcile loop, which would otherwise
// re-Ensure the namespace it is about to destroy, and to Requeue, which would
// otherwise resurrect it. Neither query mentions deletion; the tests assert it.

// Provisioning is one organisation's queue row.
type Provisioning struct {
	Org      string
	State    ProvisioningState
	Attempts int

	// LastError is the message from the most recent failure. A later success
	// does not clear it, so the row still shows what went wrong on the way.
	LastError string

	ClaimedBy        string
	ClaimedUntil     *time.Time
	NextAttemptAfter *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Claimed is what a provisioner gets when it takes a row.
type Claimed struct {
	Org string
	// Attempts is the count *including* this one. It is incremented at claim
	// time rather than on failure, so a process that dies mid-work still burns
	// an attempt and a crash loop backs off like any other failure.
	Attempts int
}

// ErrNothingToProvision reports an empty queue.
//
// A sentinel rather than a nil result, so a worker loop tells "nothing to do"
// from "something went wrong".
var ErrNothingToProvision = errors.New("nothing to provision")

// CreateOrgWithOwner records an organisation, grants its first member, and
// queues it for provisioning — all or none of them.
//
// All three rows are written in one transaction. Without the queue row nothing
// provisions the organisation and nothing reports it, because every status view
// reads the queue; without the membership /authorize answers 403 to everyone.
//
// The owner must already have an account, the same constraint `cloud member
// add` has.
func (s *Store) CreateOrgWithOwner(
	ctx context.Context, org, displayName, ownerUserID string, role identity.Role,
) error {
	if org == "" {
		return errors.New("an organisation name is required")
	}
	if ownerUserID == "" {
		return errors.New("an owner is required: an organisation nobody belongs to cannot be entered")
	}
	if !role.Valid() {
		return fmt.Errorf("%q is not a role this product understands (admin, viewer)", role)
	}

	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cloud.orgs (name, display_name) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET display_name = EXCLUDED.display_name
		`, org, displayName); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, org) DO UPDATE SET role = EXCLUDED.role
		`, ownerUserID, org, string(role)); err != nil {
			return err
		}

		// DO NOTHING, not DO UPDATE. `cloud org create` is documented as safe
		// to re-run, and an update would reset a row that is already `ready`
		// back to `pending`, re-provisioning a working organisation.
		_, err := tx.Exec(ctx, `
			INSERT INTO cloud.org_provisioning (org) VALUES ($1)
			ON CONFLICT (org) DO NOTHING
		`, org)
		return err
	})
}

// ClaimForProvisioning takes the oldest claimable organisation and leases it.
//
// Returns ErrNothingToProvision when the queue is empty, which is the ordinary
// case.
//
// A row is claimable in three states:
//
//   - 'pending': queued, never attempted.
//   - 'failed': attempted, once its backoff has passed.
//   - 'provisioning' under an expired lease, left by a provisioner that is no
//     longer running. Without this state a process that dies mid-work wedges
//     its organisation permanently, since claiming is what moved the row into
//     the state the predicate would then skip.
func (s *Store) ClaimForProvisioning(
	ctx context.Context, claimedBy string, lease time.Duration,
) (*Claimed, error) {
	if claimedBy == "" {
		// An unnamed claim is untraceable in a wedged queue.
		return nil, errors.New("a claimant name is required")
	}

	var c Claimed
	err := s.pool.QueryRow(ctx, `
		WITH ready AS (
		    SELECT org FROM cloud.org_provisioning
		     WHERE (next_attempt_after IS NULL OR next_attempt_after <= NOW())
		       AND (state IN ('pending', 'failed')
		            OR (state = 'provisioning'
		                AND (claimed_until IS NULL OR claimed_until < NOW())))
		     ORDER BY created_at
		       FOR UPDATE SKIP LOCKED
		     LIMIT 1)
		UPDATE cloud.org_provisioning p
		   SET state         = 'provisioning',
		       claimed_by    = $1,
		       claimed_until = NOW() + ($2 * interval '1 millisecond'),
		       attempts      = p.attempts + 1,
		       updated_at    = NOW()
		  FROM ready WHERE p.org = ready.org
		RETURNING p.org, p.attempts
	`, claimedBy, lease.Milliseconds()).Scan(&c.Org, &c.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNothingToProvision
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ExtendLease pushes a claim further out, for work that outlives one lease.
//
// Guarded on claimed_by, so a provisioner cannot extend a lease that has
// already expired and been taken by somebody else.
func (s *Store) ExtendLease(ctx context.Context, org, claimedBy string, lease time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET claimed_until = NOW() + ($3 * interval '1 millisecond'), updated_at = NOW()
		 WHERE org = $1 AND claimed_by = $2
	`, org, claimedBy, lease.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// MarkProvisioned records that an organisation is serving.
//
// Clears the lease so the row stops looking claimed, and clears the backoff so
// a later re-queue starts immediately rather than inheriting the wait from
// whatever went wrong last time.
func (s *Store) MarkProvisioned(ctx context.Context, org string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET state = 'ready', claimed_by = NULL, claimed_until = NULL,
		       next_attempt_after = NULL, updated_at = NOW()
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

// MarkProvisioningFailed records a failed attempt and when to try again.
//
// The caller computes the delay because the caller knows what failed: a missing
// image is worth waiting longer over than a cluster that was briefly
// unreachable. next_attempt_after is always set — left unset, the row is
// eligible again on the next tick and a permanent fault becomes a busy loop.
func (s *Store) MarkProvisioningFailed(
	ctx context.Context, org, reason string, retryIn time.Duration,
) error {
	// Checked after conversion. The query works in milliseconds, so a positive
	// duration under one rounds to zero and means "claimable immediately".
	if retryIn.Milliseconds() <= 0 {
		return errors.New("a retry delay of at least a millisecond is required: " +
			"without one a permanent failure becomes a busy loop")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET state = 'failed', last_error = $2,
		       claimed_by = NULL, claimed_until = NULL,
		       next_attempt_after = NOW() + ($3 * interval '1 millisecond'),
		       updated_at = NOW()
		 WHERE org = $1
	`, org, reason, retryIn.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// ProvisioningFor reads one organisation's queue row.
func (s *Store) ProvisioningFor(ctx context.Context, org string) (*Provisioning, error) {
	var p Provisioning
	err := s.pool.QueryRow(ctx, `
		SELECT org, state, attempts, last_error, COALESCE(claimed_by, ''),
		       claimed_until, next_attempt_after, created_at, updated_at
		  FROM cloud.org_provisioning WHERE org = $1
	`, org).Scan(&p.Org, &p.State, &p.Attempts, &p.LastError, &p.ClaimedBy,
		&p.ClaimedUntil, &p.NextAttemptAfter, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ReadyOrgs lists organisations the queue believes are serving.
//
// The input to reconciliation, and the only reader of state 'ready': the claim
// predicate skips it, so without this an organisation that stopped existing
// would stay ready permanently.
//
// Ordered by name so a reconciliation pass that is interrupted and repeated
// covers the same organisations in the same order.
func (s *Store) ReadyOrgs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT org FROM cloud.org_provisioning
		 WHERE state = 'ready'
		 ORDER BY org
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var org string
		if err := rows.Scan(&org); err != nil {
			return nil, err
		}
		out = append(out, org)
	}
	return out, rows.Err()
}

// Requeue puts a ready organisation back in the queue to be rebuilt.
//
// Guarded on state = 'ready'. Reconciliation reads a list, checks each
// organisation against the cluster, then writes; between the read and the write
// the organisation may have been deleted or claimed. Without the guard this
// would stamp 'pending' over an active claim and two provisioners would build
// the same organisation.
//
// attempts is not reset, so a row rebuilt repeatedly still shows its count.
func (s *Store) Requeue(ctx context.Context, org, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET state = 'pending', last_error = $2,
		       claimed_by = NULL, claimed_until = NULL, next_attempt_after = NULL,
		       updated_at = NOW()
		 WHERE org = $1 AND state = 'ready'
	`, org, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// The row moved between the read and here. Callers treat this
		// ErrNotFound as a pass that lost a race, not as a failure.
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}
