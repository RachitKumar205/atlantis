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
// The session window matches the console's in internal/console/store.go. The
// pending window is minutes: it spans typing a password and typing a code.
const (
	SessionTTL = 12 * time.Hour
	PendingTTL = 10 * time.Minute
)

// ErrNoSession reports a token that is not a live session.
var ErrNoSession = errors.New("not signed in")

// newToken mints 256 bits of URL-safe randomness.
//
// Same shape as the console's session token: 43 base64url characters against
// hex's 64, for the same entropy.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateSession issues a signed-in session. No code path reaches it from a
// password alone; a second factor has always been presented.
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
// Returns ErrNoSession for an unknown or expired token, and for a pending-login
// token, which lives in another table.
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
// Called after a password change, so a session established with the old
// password does not outlive it.
func (s *Store) DeleteSessionsOf(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM cloud.sessions WHERE user_id = $1`, userID)
	return err
}

// PendingLogin is a password that has been verified and a second factor that
// has not.
type PendingLogin struct {
	UserID string

	// MayEnrol distinguishes an account with no second factor, which must set
	// one up, from one that holds a factor and must present it.
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
// It does not spend it, so a wrong TOTP code costs only the code.
// SpendPendingLogin does, once the factor has been accepted.
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
// One statement, as with SpendEmailToken, so two concurrent requests presenting
// the same pending token cannot both reach a session.
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

// DeleteExpiredSessions removes sessions and pending logins that have run out.
//
// Returns both counts, including zero, so a sweep that deletes nothing is
// distinguishable from one that did not run.
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
