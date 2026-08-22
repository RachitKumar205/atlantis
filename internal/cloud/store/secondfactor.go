package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoSecondFactor reports an account with nothing enrolled.
var ErrNoSecondFactor = errors.New("no second factor is enrolled")

// ErrNoUser reports an attempt to bind an empty user.
//
// Fails closed. An empty bind leaves cloud.current_user_id() returning NULL,
// under which the RESTRICTIVE policy admits nothing — so the caller would read
// an empty result rather than an error, and "there is no second factor" is a
// much worse thing to conclude wrongly than "something failed".
var ErrNoUser = errors.New("no user bound")

// UserStore is a Store bound to one user.
//
// # Why this exists now and not in C1
//
// C1 had no table that could carry a per-user boundary — its four were all
// either lookups that *discover* who a request is, or registries. A bound
// handle guarding nothing would have been an abstraction with no purpose, so it
// was left out.
//
// cloud.totp_secrets and cloud.backup_codes changed that: both are read only
// for a user who has already been identified. The row-level-security policy is
// the actual boundary; this is what makes it reachable without every call site
// remembering to bind. A caller that has not said who it is acting for cannot
// call these methods at all, and that is a compile error rather than an empty
// result.
type UserStore struct {
	db     *Store
	userID string
}

// ForUser returns a handle scoped to one user.
//
// Built from an authenticated session or a verified pending login — never from
// a request parameter, which would let the caller choose whose second factor to
// read.
func (s *Store) ForUser(userID string) *UserStore { return &UserStore{db: s, userID: userID} }

// tx runs fn inside a transaction with the user bound.
//
// A transaction is required rather than convenient: cloud.set_user uses
// set_config(..., true), which is transaction-local. That is what makes it safe
// on a pooled connection — the value reverts when the transaction ends, so the
// next request to borrow the same backend cannot inherit it. A plain SET on the
// pool would persist into whatever unrelated request came next.
func (u *UserStore) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	if u.userID == "" {
		return ErrNoUser
	}

	tx, err := u.db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Rollback on a fresh context, not the request's. pgx destroys the
		// pooled connection when a rollback Exec fails, so rolling back on a
		// cancelled context churns the pool — and a cancelled request is the
		// common case, not an exotic one.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txFinishTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The bind must be the first statement. Anything issued ahead of it runs
	// unscoped, and under the RESTRICTIVE policy that means reading nothing —
	// which looks like "no second factor enrolled" rather than like a bug.
	if _, err := tx.Exec(ctx, `SELECT cloud.set_user($1)`, u.userID); err != nil {
		return fmt.Errorf("bind user: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txFinishTimeout)
	defer cancel()
	if err := tx.Commit(cctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// ── TOTP ────────────────────────────────────────────────────────────────────

// PutTOTPSecret stores an unconfirmed secret, replacing any previous one.
//
// Unconfirmed because enrolment is not finished until the user has proved they
// can generate a code from it. Storing it confirmed would let somebody scan a
// code, close the page, and be locked out by a factor they never captured.
func (u *UserStore) PutTOTPSecret(ctx context.Context, ciphertext []byte) error {
	return u.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO cloud.totp_secrets (user_id, secret_ct) VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE
			   SET secret_ct = EXCLUDED.secret_ct,
			       confirmed_at = NULL,
			       last_used_step = NULL
		`, u.userID, ciphertext)
		return err
	})
}

// TOTPSecret returns the stored ciphertext and whether enrolment finished.
func (u *UserStore) TOTPSecret(ctx context.Context) (ciphertext []byte, confirmed bool, err error) {
	err = u.tx(ctx, func(tx pgx.Tx) error {
		var confirmedAt *time.Time
		e := tx.QueryRow(ctx,
			`SELECT secret_ct, confirmed_at FROM cloud.totp_secrets WHERE user_id = $1`,
			u.userID).Scan(&ciphertext, &confirmedAt)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNoSecondFactor
		}
		confirmed = confirmedAt != nil
		return e
	})
	return ciphertext, confirmed, err
}

// ConfirmTOTP finishes enrolment and records the step the proving code came
// from, so that same code cannot immediately be replayed.
func (u *UserStore) ConfirmTOTP(ctx context.Context, step int64) error {
	return u.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE cloud.totp_secrets
			   SET confirmed_at = NOW(), last_used_step = $2
			 WHERE user_id = $1
		`, u.userID, step)
		return err
	})
}

// SpendTOTPStep records that a code from this step has been accepted, and
// reports whether it was already spent.
//
// # Why a step counter rather than nothing
//
// A TOTP code is valid for its whole 30-second window, and by default for the
// window either side of it. Without this, a code observed over a shoulder or
// captured by a phishing proxy can be presented again for the rest of that
// window — which is ample. The comparison is `>=` rather than `=` so an
// out-of-order or replayed *earlier* step is refused too.
//
// One statement, so two concurrent presentations of the same code cannot both
// win the check and both proceed.
func (u *UserStore) SpendTOTPStep(ctx context.Context, step int64) (bool, error) {
	spent := false
	err := u.tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `
			UPDATE cloud.totp_secrets
			   SET last_used_step = $2
			 WHERE user_id = $1
			   AND (last_used_step IS NULL OR last_used_step < $2)
		`, u.userID, step)
		if e != nil {
			return e
		}
		spent = tag.RowsAffected() == 1
		return nil
	})
	return spent, err
}

// ── Backup codes ────────────────────────────────────────────────────────────

// ReplaceBackupCodes discards any existing codes and stores a fresh set of
// hashes.
//
// Replacing rather than appending: a user regenerating their codes expects the
// old printout to stop working, and a set that accumulated would mean a code
// from a sheet thrown away years ago still opens the account.
func (u *UserStore) ReplaceBackupCodes(ctx context.Context, hashes []string) error {
	return u.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM cloud.backup_codes WHERE user_id = $1`, u.userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(ctx,
				`INSERT INTO cloud.backup_codes (user_id, code_hash) VALUES ($1, $2)`,
				u.userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnusedBackupCodeHashes returns the hashes still available to spend.
//
// The caller compares the presented code against each, because the hash is
// salted per code and cannot be looked up. Ten argon2id verifications is around
// half a second, on an operation used approximately once in an account's life.
func (u *UserStore) UnusedBackupCodeHashes(ctx context.Context) (map[int64]string, error) {
	out := map[int64]string{}
	err := u.tx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx,
			`SELECT id, code_hash FROM cloud.backup_codes
			  WHERE user_id = $1 AND used_at IS NULL ORDER BY id`, u.userID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var hash string
			if err := rows.Scan(&id, &hash); err != nil {
				return err
			}
			out[id] = hash
		}
		return rows.Err()
	})
	return out, err
}

// SpendBackupCode marks one code used, reporting whether it was still unused.
//
// The check and the write are one statement for the same reason as everywhere
// else here: two requests presenting the same code must not both succeed.
func (u *UserStore) SpendBackupCode(ctx context.Context, id int64) (bool, error) {
	spent := false
	err := u.tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `
			UPDATE cloud.backup_codes SET used_at = NOW()
			 WHERE id = $1 AND user_id = $2 AND used_at IS NULL
		`, id, u.userID)
		if e != nil {
			return e
		}
		spent = tag.RowsAffected() == 1
		return nil
	})
	return spent, err
}

// HasConfirmedFactor reports whether this account can complete a sign-in.
//
// # Why this is on the bound handle
//
// It was written unscoped, on the bare pool, with a comment arguing that was
// fine because it returns only a boolean. That argument was wrong, and a test
// caught it: cloud.totp_secrets is policed, so an unbound read matches nothing
// and the count is always zero. Every sign-in therefore reported "no second
// factor enrolled" and handed out a pending login that could ENROL one —
// letting anybody holding just a password replace the second factor on an
// account that already had one.
//
// The failure was silent in exactly the way the migration's own notes predicted:
// reading nothing through a policy looks like an empty table rather than like a
// missing bind.
//
// There is no bootstrap problem here to justify going around the boundary. This
// is asked after the password has been verified, so the user is already known.
func (u *UserStore) HasConfirmedFactor(ctx context.Context) (bool, error) {
	found := false
	err := u.tx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM cloud.totp_secrets
			 WHERE user_id = $1 AND confirmed_at IS NOT NULL
		`, u.userID).Scan(&n); err != nil {
			return err
		}
		found = n > 0
		return nil
	})
	return found, err
}
