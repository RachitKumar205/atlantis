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
// An empty bind leaves cloud.current_user_id() NULL, under which the
// RESTRICTIVE policy admits nothing, so the caller reads an empty result rather
// than an error and concludes there is no second factor.
var ErrNoUser = errors.New("no user bound")

// UserStore is a Store bound to one user. Its methods query cloud.totp_secrets
// and cloud.backup_codes, whose row-level-security policies are the boundary;
// this handle sets `cloud.user` so those policies match, and makes an unbound
// query a compile error rather than an empty result.
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
// A transaction is required: cloud.set_user uses set_config(..., true), which
// is transaction-local, so the value reverts when the transaction ends and the
// next request to borrow the backend cannot inherit it.
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
		// pooled connection when a rollback Exec fails, so rolling back on an
		// already-cancelled context churns the pool.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txFinishTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// The bind must be the first statement. Anything ahead of it runs unscoped
	// and reads nothing under the RESTRICTIVE policy.
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

// PutTOTPSecret stores an unconfirmed secret, replacing any previous one.
//
// Unconfirmed until a code proves the authenticator captured it. Storing it
// confirmed gates the account behind a factor no authenticator holds.
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
// A TOTP code is valid for its whole 30-second window and, by default, for the
// window either side, so without a spent-step record a captured code can be
// replayed for the rest of that period. The comparison is >= rather than =, so
// an earlier step is refused as well as a repeated one.
//
// One statement, so two concurrent presentations of the same code cannot both
// pass.
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

// ReplaceBackupCodes discards any existing codes and stores a fresh set of
// hashes.
//
// Replacing rather than appending, so a discarded printout stops working.
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
// half a second.
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
// The check and the write are one statement, so two requests presenting the
// same code cannot both succeed.
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
// On the bound handle, not the pool. cloud.totp_secrets is policed, so an
// unbound count reads zero rather than failing, and every sign-in would issue a
// pending login permitted to enrol a replacement factor.
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
