package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session and pending-login lifetimes.
//
// The session window matches the console's (internal/console/store.go): long
// enough to cover a working day without re-authenticating, short enough that a
// stolen cookie has a bounded life. The pending window is minutes, because it
// is the gap between typing a password and typing a code — anything longer is
// a half-authenticated credential sitting around for no reason.
const (
	SessionTTL = 12 * time.Hour
	PendingTTL = 10 * time.Minute
)

// ErrNoSession reports a token that is not a live session.
var ErrNoSession = errors.New("not signed in")

// newToken mints 256 bits of URL-safe randomness.
//
// Same shape as the console's session token: 43 characters rather than the 64 a
// hex encoding would take, for the same entropy.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ── Sessions ────────────────────────────────────────────────────────────────

// CreateSession issues a signed-in session.
//
// Only ever called after a second factor has been presented. There is no code
// path from a password to this function, which is the property the whole
// two-table split exists to make structural rather than remembered.
func (s *Store) CreateSession(ctx context.Context, userID string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO cloud.sessions (token, user_id, expires_at) VALUES ($1, $2, $3)
	`, token, userID, time.Now().Add(SessionTTL))
	if err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser resolves a session token to its account.
//
// Returns ErrNoSession for an unknown or expired token — and, importantly, for
// a *pending login* token, because a pending login is not in this table. That
// is the point of the split: no check has to be remembered here.
func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM cloud.sessions WHERE token = $1 AND expires_at > NOW()`,
		token).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}

// DeleteSession signs one browser out. Not an error if it was already gone —
// signing out twice is a thing browsers do, and reporting it as a failure tells
// the user something went wrong when nothing did.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cloud.sessions WHERE token = $1`, token)
	return err
}

// DeleteSessionsOf signs every browser out for one account.
//
// Called after a password change, so that a session established with the old
// password does not outlive it. Without this, changing a password because
// somebody else knows it leaves them signed in.
func (s *Store) DeleteSessionsOf(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cloud.sessions WHERE user_id = $1`, userID)
	return err
}

// ── Half-finished logins ────────────────────────────────────────────────────

// PendingLogin is a password that has been verified and a second factor that
// has not.
type PendingLogin struct {
	UserID string

	// MayEnrol distinguishes an account with no second factor, which must set
	// one up, from one that has a factor and must present it. Both are
	// half-finished; neither is a session.
	MayEnrol bool
}

// CreatePendingLogin records a verified password.
func (s *Store) CreatePendingLogin(ctx context.Context, userID string, mayEnrol bool) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO cloud.pending_logins (token, user_id, may_enrol, expires_at)
		VALUES ($1, $2, $3, $4)
	`, token, userID, mayEnrol, time.Now().Add(PendingTTL))
	if err != nil {
		return "", err
	}
	return token, nil
}

// PendingLoginFor resolves a half-finished login.
//
// Deliberately does NOT spend it. A wrong TOTP code should not cost the user
// their whole login — they retype the code. What spends it is
// SpendPendingLogin, called only when the factor has been accepted.
func (s *Store) PendingLoginFor(ctx context.Context, token string) (*PendingLogin, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	var p PendingLogin
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, may_enrol FROM cloud.pending_logins
		 WHERE token = $1 AND expires_at > NOW()
	`, token).Scan(&p.UserID, &p.MayEnrol)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SpendPendingLogin consumes a half-finished login and returns the account it
// belonged to.
//
// One statement, so the check and the spend cannot race — the same reasoning as
// SpendEmailToken. Two concurrent requests presenting one pending token must
// not both proceed to create a session.
func (s *Store) SpendPendingLogin(ctx context.Context, token string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		DELETE FROM cloud.pending_logins
		 WHERE token = $1 AND expires_at > NOW()
		RETURNING user_id
	`, token).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSession
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// ── Expiry ──────────────────────────────────────────────────────────────────

// DeleteExpiredSessions removes sessions and pending logins that have run out.
//
// Returns both counts so the caller can log them, including zero. A sweep that
// silently deletes nothing forever is a shape this repository has shipped
// before — see the TTL sweeper in the CHANGELOG.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (sessions, pending int64, err error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM cloud.sessions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, 0, err
	}
	sessions = tag.RowsAffected()

	tag, err = s.pool.Exec(ctx, `DELETE FROM cloud.pending_logins WHERE expires_at < NOW()`)
	if err != nil {
		return sessions, 0, err
	}
	return sessions, tag.RowsAffected(), nil
}
