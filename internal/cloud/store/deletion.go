package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// Deleting an organisation, in two steps that are deliberately far apart.
//
// SoftDeleteOrg stops it serving and starts a clock. Nothing in the cluster is
// touched. ClaimForPurge hands it to a provisioner once that clock runs out,
// and only then does anything get destroyed.
//
// The gap exists because there are no database backups yet. Destroying on the
// button press would take a customer's namespace, Postgres cluster and volume
// with nothing anywhere to restore from — so the window is the only safety net
// the system currently has, and it is not an ornament on the feature.
//
// # Nothing here touches Kubernetes
//
// Every function in this file writes a row. The provisioner is the only process
// that destroys anything, for the reason stated in internal/provisioner's
// package comment: Cloud holds every password, every TOTP secret and the
// assertion signing key, and that risk was accepted on the understanding that
// script on Cloud's origin could reach /api/account/*, not that it could
// schedule pods. Or delete them.

// ErrNothingToPurge is returned when no organisation is due for destruction.
// The ordinary case, and not a failure — the same shape as
// ErrNothingToProvision.
var ErrNothingToPurge = errors.New("nothing to purge")

// ErrNotPermitted is returned when the caller is a member but not an admin.
//
// Distinct from ErrNotFound, which is what a non-member gets. The difference is
// deliberate: telling a viewer "you may not delete this" is fine, because they
// already know the organisation exists. Telling a stranger the same thing would
// report which organisations exist to anybody who asked.
var ErrNotPermitted = errors.New("this account may not do that")

// SoftDeleteOrg stops an organisation serving and schedules its destruction.
//
// # What it does not do
//
// It does not touch the cluster. The namespace, the Postgres cluster, the
// volume and the certificate authority are all exactly as they were, which is
// what makes RestoreOrg a real restore rather than a rebuild — a rebuild would
// mint a new authority and force every enrolled caller to run `tide login`
// again.
//
// # The guards
//
// Guarded on `state = 'ready'`, so an organisation that is already deleted
// cannot be deleted twice. Without that a second call would move purge_after
// further out, quietly extending a window that somebody is counting on, and a
// caller retrying a request they thought had failed would do it by accident.
//
// Guarded on an admin membership rather than on cloud.orgs.created_by. Ownership
// would be the narrower rule, but created_by is nullable — ON DELETE SET NULL,
// so an organisation whose creator closed their account has nobody who owns it
// — and that leaves an organisation nobody can delete. Any admin can, which is
// wider and has no orphan case.
//
// window is passed rather than read from a constant here because it is a
// promise made at the moment of deletion. Storing purge_after rather than
// recomputing it means changing the default later cannot silently move the
// destruction date of an organisation already in the window.
func (s *Store) SoftDeleteOrg(ctx context.Context, org, userID string, window time.Duration) error {
	if org == "" || userID == "" {
		return errors.New("an organisation and an account are both required")
	}
	if window <= 0 {
		// A window of zero is a hard delete wearing the shape of a soft one.
		// If that is ever wanted it should be PurgeNow, where it is legible.
		return errors.New("a positive retention window is required: " +
			"a zero window destroys immediately, which is what PurgeNow is for")
	}

	return s.tx(ctx, func(tx pgx.Tx) error {
		// Membership first, so a non-member and a viewer get different answers.
		var role identity.Role
		err := tx.QueryRow(ctx, `
			SELECT role FROM cloud.memberships WHERE user_id = $1 AND org = $2
		`, userID, org).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", org, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if role != identity.RoleAdmin {
			return fmt.Errorf("%s: %w", org, ErrNotPermitted)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE cloud.org_provisioning
			   SET state       = 'deleted',
			       deleted_at  = NOW(),
			       purge_after = NOW() + ($2 * interval '1 millisecond'),
			       claimed_by = NULL, claimed_until = NULL,
			       updated_at  = NOW()
			 WHERE org = $1 AND state = 'ready'
		`, org, window.Milliseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Either it does not exist or it is not ready — mid-provision,
			// failed, or already deleted. Not distinguished, because the
			// caller's next move is the same in every case: look at the state.
			return fmt.Errorf("%s is not in a state that can be deleted: %w", org, ErrNotFound)
		}
		return nil
	})
}

// RestoreOrg returns a soft-deleted organisation to service.
//
// The namespace was never touched, so this is genuinely a restore: the same
// certificate authority, the same database, the same data. Every caller that
// enrolled before the deletion keeps working.
//
// Guarded on `state = 'deleted'`, which is what stops this being a way to force
// an organisation stuck in `provisioning` or `purging` back to `ready` — the
// second of those would hand a live namespace back to a customer while a
// provisioner was midway through destroying it.
func (s *Store) RestoreOrg(ctx context.Context, org, userID string) error {
	if org == "" || userID == "" {
		return errors.New("an organisation and an account are both required")
	}

	return s.tx(ctx, func(tx pgx.Tx) error {
		var role identity.Role
		err := tx.QueryRow(ctx, `
			SELECT role FROM cloud.memberships WHERE user_id = $1 AND org = $2
		`, userID, org).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: %w", org, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if role != identity.RoleAdmin {
			return fmt.Errorf("%s: %w", org, ErrNotPermitted)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE cloud.org_provisioning
			   SET state = 'ready', deleted_at = NULL, purge_after = NULL,
			       updated_at = NOW()
			 WHERE org = $1 AND state = 'deleted'
		`, org)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%s is not deleted: %w", org, ErrNotFound)
		}
		return nil
	})
}

// PurgeNow brings the destruction forward to immediately.
//
// The operator hatch, reached from `cloud org purge`. It exists because a
// deleted organisation keeps its row for the whole window, and that row holds
// the name and counts against the creator's limit — so "delete it and make a
// fresh one with the same name" is otherwise a thirty-day wait.
//
// Unguarded by role on purpose: the caller is somebody with the Cloud database
// URL and a shell, and a membership check would be theatre. The guard on this
// one lives in the command, which requires the organisation named as an
// argument and an explicit flag.
//
// Works from any state, including `ready`. An operator purging an organisation
// that is still serving is doing something drastic and knows it; refusing would
// mean the escape hatch does not work in the case somebody most needs it.
func (s *Store) PurgeNow(ctx context.Context, org string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET state       = 'deleted',
		       deleted_at  = COALESCE(deleted_at, NOW()),
		       purge_after = NOW(),
		       claimed_by = NULL, claimed_until = NULL,
		       updated_at  = NOW()
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

// ClaimForPurge takes one organisation that is due for destruction and leases
// it.
//
// # Why this is not ClaimForProvisioning with a wider predicate
//
// That query already carries a comment saying three things make a row claimable
// and the third is the one that is easy to leave out. Adding a fourth dimension
// and a CASE over the target state would make the one query nobody can afford
// to get wrong harder to read.
//
// They also differ in more than their state set. Provisioning is slow and backs
// off on failure; teardown is fast and should retry promptly. One lease value
// and one retry policy cannot be right for both.
//
// # The predicate
//
// Two ways in, and the second is the one that matters after a crash:
//
//   - `deleted` with purge_after in the past. The window has run out.
//   - `purging` whose lease has expired. A provisioner died mid-teardown, and
//     the organisation would otherwise sit half-destroyed for ever.
//
// purge_after IS NOT NULL is stated even though `deleted` always has one set,
// because a NULL comparison is false rather than an error and the row would
// simply never be claimed — a reaper that silently does nothing is the worst
// possible failure here.
func (s *Store) ClaimForPurge(
	ctx context.Context, claimedBy string, lease time.Duration,
) (*Claimed, error) {
	if claimedBy == "" {
		return nil, errors.New("a claimant name is required")
	}

	var c Claimed
	err := s.pool.QueryRow(ctx, `
		WITH due AS (
		    SELECT org FROM cloud.org_provisioning
		     WHERE (state = 'deleted'
		            AND purge_after IS NOT NULL
		            AND purge_after <= NOW())
		        OR (state = 'purging'
		            AND (claimed_until IS NULL OR claimed_until < NOW()))
		     ORDER BY purge_after
		       FOR UPDATE SKIP LOCKED
		     LIMIT 1)
		UPDATE cloud.org_provisioning p
		   SET state         = 'purging',
		       claimed_by    = $1,
		       claimed_until = NOW() + ($2 * interval '1 millisecond'),
		       updated_at    = NOW()
		  FROM due WHERE p.org = due.org
		RETURNING p.org, p.attempts
	`, claimedBy, lease.Milliseconds()).Scan(&c.Org, &c.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNothingToPurge
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkPurged removes every trace of an organisation.
//
// Called only after the cluster teardown has succeeded. The order is
// deliberate: memberships and the provisioning row reference cloud.orgs, so the
// organisation itself goes last.
//
// This is the point of no return, and it is one statement. A partial purge —
// the namespace gone but the rows still present — would leave an organisation
// that appears in somebody's list, reports `purging` for ever, and has nothing
// behind it.
func (s *Store) MarkPurged(ctx context.Context, org string) error {
	if org == "" {
		return errors.New("an organisation is required")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM cloud.org_provisioning WHERE org = $1`, org); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM cloud.memberships WHERE org = $1`, org); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM cloud.orgs WHERE name = $1`, org)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%s: %w", org, ErrNotFound)
		}
		return nil
	})
}

// MarkPurgeFailed releases a claim so the teardown is tried again.
//
// Back to `deleted` rather than to a failure state, and purge_after is left
// where it is — already in the past — so the next reap pass picks it up. There
// is deliberately no backoff and no attempt cap: an organisation whose teardown
// keeps failing must keep being tried, because the alternative is a customer
// who asked to be deleted and quietly was not.
//
// The failure is recorded in last_error, which is what an operator reads.
func (s *Store) MarkPurgeFailed(ctx context.Context, org, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET state = 'deleted', last_error = $2,
		       claimed_by = NULL, claimed_until = NULL,
		       updated_at = NOW()
		 WHERE org = $1
	`, org, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}
