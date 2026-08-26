package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Token purposes. A token issued for one does not satisfy the other: they are
// issued under different conditions, verification at sign-up to an unproven
// address and reset to a proven one.
const (
	PurposeVerifyEmail   = "verify_email"
	PurposeResetPassword = "reset_password"
)

// Token lifetimes.
//
// Verification is generous: an address may not be read for a day. Reset is
// short, because a reset link sitting in an inbox is a live credential.
const (
	VerifyTokenTTL = 24 * time.Hour
	ResetTokenTTL  = 1 * time.Hour
)

// ErrTokenInvalid reports a token that is unknown, expired, already spent, or
// issued for a different purpose.
//
// One error for all four: distinguishing them would let a caller probe for
// valid tokens, and every case has the same remedy.
var ErrTokenInvalid = errors.New("this link is not valid any more")

// IssueEmailToken mints a token, stores its hash, and returns the token.
//
// The database holds a SHA-256 of the token, so a read of cloud.email_tokens is
// not a working reset for every pending account.
func (s *Store) IssueEmailToken(ctx context.Context, userID, email, purpose string, ttl time.Duration) (string, error) {
	if purpose != PurposeVerifyEmail && purpose != PurposeResetPassword {
		return "", fmt.Errorf("unknown token purpose %q", purpose)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	_, err := s.pool.Exec(ctx, `
		INSERT INTO cloud.email_tokens (token_hash, user_id, purpose, email, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, sum[:], userID, purpose, NormalizeEmail(email), time.Now().Add(ttl))
	if err != nil {
		return "", err
	}
	return token, nil
}

// SpentToken is what a successfully spent token resolves to.
type SpentToken struct {
	UserID string
	Email  string
}

// SpendEmailToken validates a token for one purpose and marks it used.
//
// The check and the spend are one statement: UPDATE ... WHERE used_at IS NULL
// ... RETURNING, with the row count as the answer. Reading and then updating
// leaves a window in which two concurrent requests both see the token unused.
//
// The address is compared as well as the user, so a token issued to an address
// the account no longer holds cannot be spent.
func (s *Store) SpendEmailToken(ctx context.Context, token, purpose string) (*SpentToken, error) {
	sum := sha256.Sum256([]byte(token))

	var out SpentToken
	err := s.pool.QueryRow(ctx, `
		UPDATE cloud.email_tokens t
		   SET used_at = NOW()
		  FROM cloud.users u
		 WHERE t.token_hash = $1
		   AND t.purpose    = $2
		   AND t.used_at IS NULL
		   AND t.expires_at > NOW()
		   AND u.id    = t.user_id
		   AND u.email = t.email
		RETURNING t.user_id, t.email
	`, sum[:], purpose).Scan(&out.UserID, &out.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// InvalidateEmailTokens spends every outstanding token of one purpose for a
// user.
//
// Called after a password is set, so any other reset link in flight stops
// working. Three requests otherwise leave three live credentials.
func (s *Store) InvalidateEmailTokens(ctx context.Context, userID, purpose string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE cloud.email_tokens SET used_at = NOW()
		 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL
	`, userID, purpose)
	return err
}

// DeleteExpiredEmailTokens removes rows that can no longer be spent.
//
// Rows are kept until expiry rather than deleted on use, so a second
// presentation of the same link is distinguishable from one that never existed.
func (s *Store) DeleteExpiredEmailTokens(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cloud.email_tokens WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetPassword stores a hash, which the caller has already produced.
//
// Takes a hash rather than a password. This package does not import the hashing
// package, so no code path carries a plaintext password into a SQL statement.
// The type does not enforce it; the parameter name does.
func (s *Store) SetPassword(ctx context.Context, userID, passwordHash string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cloud.users SET password_hash = $2, updated_at = NOW() WHERE id = $1`,
		userID, passwordHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkEmailVerified records that an address has been proven.
func (s *Store) MarkEmailVerified(ctx context.Context, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE cloud.users SET email_verified_at = NOW(), updated_at = NOW()
		  WHERE id = $1 AND email_verified_at IS NULL`,
		userID)
	if err != nil {
		return err
	}
	// Zero rows means either no such user or already verified. The second is
	// not a failure — following a verification link twice should not report an
	// error — so the caller is told nothing changed and decides.
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
