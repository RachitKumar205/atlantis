package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// Deleting an organisation takes two steps, separated by a retention window.
// SoftDeleteOrg stops it serving and sets purge_after; ClaimForPurge hands it
// to a provisioner once purge_after has passed.
//
// The window is the only recovery path: there are no database backups, so
// destroying on the request would take the namespace, Postgres cluster and
// volume with nothing to restore from.
//
// Every function in this file writes a row. Only the provisioner destroys
// anything.

// ErrNothingToPurge is returned when no organisation is due for destruction,
// which is the ordinary case.
var ErrNothingToPurge = errors.New("nothing to purge")

// ErrNotPermitted is returned when the caller is a member but not an admin. A
// non-member gets ErrNotFound instead: a member already knows the organisation
// exists, and reporting the difference to a non-member would enumerate
// organisations.
var ErrNotPermitted = errors.New("this account may not do that")

// SoftDeleteOrg stops an organisation serving and schedules its destruction.
//
// The cluster is untouched: namespace, Postgres cluster, volume and certificate
// authority all survive, so RestoreOrg keeps the authority every caller
// enrolled against.
//
// Guarded on state = 'ready', so a second call cannot push purge_after further
// out. Guarded on an admin membership rather than cloud.orgs.created_by, which
// is nullable under ON DELETE SET NULL: an organisation whose creator closed
// their account would then be undeletable.
//
// purge_after is stored rather than recomputed from window at purge time, so
// changing the default cannot move the destruction date of an organisation
// already inside its window.
func (s *Store) SoftDeleteOrg(ctx context.Context, org, userID string, window time.Duration) error {
	if org == "" || userID == "" {
		return errors.New("an organisation and an account are both required")
	}
	if window <= 0 {
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
			       -- An outstanding image roll does not survive deletion. The
			       -- provisioner skips an organisation that is not ready, so a
			       -- request kept here would do nothing until a restore, and
			       -- then restart the database on an intent weeks old.
			       image_roll_requested_at = NULL,
			       image_roll_kinds        = NULL,
			       image_roll_attempts     = 0,
			       updated_at  = NOW()
			 WHERE org = $1 AND state = 'ready'
		`, org, window.Milliseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Either it does not exist or it is not ready — mid-provision,
			// failed, or already deleted. Not distinguished: the caller's next
			// move in every case is to look at the state.
			return fmt.Errorf("%s is not in a state that can be deleted: %w", org, ErrNotFound)
		}
		return nil
	})
}

// RestoreOrg returns a soft-deleted organisation to service.
//
// The namespace was never touched: the same certificate authority, the same
// database, the same data, so every caller that enrolled before the deletion
// keeps working.
//
// Guarded on state = 'deleted'. Without it this would force an organisation
// stuck in 'provisioning' or 'purging' back to 'ready', handing a live
// namespace back while a provisioner was midway through destroying it.
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
// The operator hatch, reached from `cloud org purge`. A deleted organisation
// keeps its row for the whole window, and that row holds the name and counts
// against the creator's limit, so remaking it under the same name is otherwise
// a thirty-day wait.
//
// No role check: reaching this requires the Cloud database URL. The guard is in
// the command, which requires the organisation as an argument and an explicit
// flag.
//
// Works from any state, including 'ready'.
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
// Separate from ClaimForProvisioning: provisioning is slow and backs off on
// failure, teardown is fast and retries promptly, so the two take different
// lease values.
//
// A row is claimable two ways:
//
//   - state 'deleted' with purge_after in the past.
//   - state 'purging' under an expired lease, left by a provisioner that died
//     mid-teardown.
//
// purge_after IS NOT NULL is stated even though 'deleted' always sets it: a
// NULL comparison yields false rather than an error, so the row would never be
// claimed and nothing would report it.
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
// Called only after the cluster teardown has succeeded. memberships and the
// provisioning row reference cloud.orgs, so the organisation row goes last.
//
// One statement. A partial purge leaves rows for an organisation whose
// namespace is gone: it still appears in listings and reports 'purging'
// permanently.
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
// Returns to 'deleted' rather than a failure state, leaving purge_after in the
// past so the next reap pass claims it. There is no backoff and no attempt cap:
// a teardown that stops being retried leaves data that was asked to be deleted.
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
