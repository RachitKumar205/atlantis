package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// ── The provisioning queue ──────────────────────────────────────────────────
//
// What decides an organisation should have an atlantis, and what remembers how
// that went. Migration 0005 has the table and the reasoning; this is the Go
// side of it.
//
// The shape is atlantis.jobs', deliberately, because that is the lease queue
// this codebase already runs and getting a second one subtly different helps
// nobody. Two properties are load-bearing and neither is obvious:
//
//   - The claim is ONE statement. A select followed by an update has a window
//     between them in which another provisioner can take the same row; a CTE
//     holding FOR UPDATE SKIP LOCKED feeding the update has none.
//
//   - A lease, not a flag. What makes a row claimable again is claimed_until
//     passing, so a provisioner that dies mid-work frees its row by doing
//     nothing at all. There is no sweeper to write, and none to forget to run.

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
)

// Provisioning is one organisation's queue row.
type Provisioning struct {
	Org      string
	State    ProvisioningState
	Attempts int

	// LastError is the message from the most recent failure, kept after a
	// later success rather than cleared: "this took four goes and here is what
	// was wrong" is worth more than a tidy row.
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
// A sentinel rather than a nil result, because "nothing to do" and "something
// went wrong" are answers a worker loop must tell apart, and a nil-check is the
// easiest thing in the world to get the wrong way round.
var ErrNothingToProvision = errors.New("nothing to provision")

// CreateOrgWithOwner records an organisation, grants its first member, and
// queues it for provisioning — all or none of them.
//
// # Why these three are one write
//
// Each pair without the third is a state the product cannot recover from on its
// own:
//
//   - An organisation with no queue row is provisioned by nobody, for ever, and
//     nothing anywhere reports it. There is no row on any screen to show it,
//     because the screen reads the queue.
//   - An organisation with no member cannot be entered by anyone. /authorize
//     checks membership before it checks anything else, so a perfectly
//     provisioned organisation still answers 403.
//
// The owner must already have an account. That is the same constraint `cloud
// member add` has, and the right one: an organisation owned by an address
// nobody has verified is an organisation nobody can enter.
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

		// DO NOTHING, not DO UPDATE. Re-running this for an existing
		// organisation must not reset a row that is already `ready` back to
		// `pending` — that would re-provision a working organisation, and
		// `cloud org create` is documented as safe to re-run.
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
// case and not a failure.
//
// # Reading the predicate
//
// Three things make a row claimable, and the third is the one that is easy to
// leave out:
//
//   - pending — queued, never attempted.
//   - failed — attempted and did not finish, once its backoff has passed.
//   - provisioning WITH AN EXPIRED LEASE — claimed by a provisioner that is no
//     longer running. Omitting this state looks harmless and means a process
//     that dies mid-work wedges its organisation for ever, because claiming is
//     what put it in the state the predicate then refuses to look at.
func (s *Store) ClaimForProvisioning(
	ctx context.Context, claimedBy string, lease time.Duration,
) (*Claimed, error) {
	if claimedBy == "" {
		// Not load-bearing for correctness — the lease is — but an unnamed
		// claim is one an operator staring at a wedged queue cannot trace.
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
// already expired and been taken by somebody else — which is precisely when it
// would most want to.
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
// unreachable. What this refuses to do is leave next_attempt_after unset —
// atlantis.jobs does exactly that, writing last_error_at and never reading it,
// and the result is a failed row eligible again on the very next tick. A
// permanent fault then becomes a loop that provisions nothing and fills the log.
func (s *Store) MarkProvisioningFailed(
	ctx context.Context, org, reason string, retryIn time.Duration,
) error {
	// Checked after conversion, not before. The query works in milliseconds, so
	// anything under one rounds to zero and means "claimable immediately" —
	// which is the busy loop this guard exists to prevent, arrived at through a
	// value that looked positive.
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
// The input to reconciliation. Nothing else reads this state: the claim
// predicate deliberately ignores 'ready', so without a caller here an
// organisation that stops existing stays ready for ever.
//
// Ordered by name rather than by time so a reconciliation pass that is
// interrupted and repeated covers the same organisations in the same order,
// which makes "it always gets stuck on the same one" a visible symptom rather
// than a coincidence.
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
// # Why this is guarded on the ready state
//
// Reconciliation reads a list, then checks each organisation against the
// cluster, then writes. Between the read and the write an operator may have
// deleted the organisation, or a provisioner may have claimed it after somebody
// reset it by hand. Without `state = 'ready'` in the predicate this would
// stamp 'pending' over a claim that is actively being worked, and two
// provisioners would build the same organisation.
//
// attempts is deliberately not reset. A row that has been rebuilt four times is
// telling you something, and clearing the count hides it.
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
		// Not an error worth failing a reconciliation pass over: it means the
		// row moved while we were looking at the cluster, which is the case the
		// predicate exists to lose safely.
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}
