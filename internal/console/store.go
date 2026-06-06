package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func jsonMarshalBytes(v any) ([]byte, error) { return json.Marshal(v) }

// sessionTTL is the maximum age of an idle session. Active sessions are
// extended on each authenticated request (see store.touchSession) up to
// this window, so an actively-used console stays signed in indefinitely
// while an inactive cookie dies within sessionTTL of last use.
//
// 12 hours is the standard admin-console choice — long enough to cover a
// working day without re-auth, short enough that a stolen cookie has
// bounded lifetime.
const sessionTTL = 12 * time.Hour

// sessionTouchThreshold defines the "sliding renewal" zone. When an
// authenticated request comes in and the session's remaining TTL is
// below this fraction of the full window, we bump expires_at. This
// throttles renewal writes — at 0.5 with 12h TTL, an active user incurs
// roughly one renewal write every 6h, not one per request.
const sessionTouchThreshold = 0.5

// sudoTTL is how long a successful re-auth keeps the session in
// "sudo mode" for destructive actions (sign-out-all, revoke-all).
// Short window so a logged-in laptop walked-away-from can't escalate.
const sudoTTL = 5 * time.Minute

// CallerRepo maps a caller name to its GitHub repository.
type CallerRepo struct {
	Caller           string
	Owner            string
	Repo             string
	DefaultBranch    string
	SchemaPathPrefix string
}

// ErrNotFound is returned when a user or session is not found, or when
// credentials are invalid. Callers must not distinguish the two cases to
// avoid user enumeration.
var ErrNotFound = errors.New("not found")

type User struct {
	ID        int64
	Email     string
	Role      string // "admin" | "viewer"
	FirstName string
	LastName  string
	CreatedAt time.Time
}

type store struct {
	pool *pgxpool.Pool
}

func newStore(ctx context.Context, pgURL string) (*store, error) {
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool}, nil
}

func (s *store) close() { s.pool.Close() }

// migrate creates the console schema + users and sessions tables. Runs at
// startup; idempotent via CREATE IF NOT EXISTS.
func (s *store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS console;

		CREATE TABLE IF NOT EXISTS console.users (
			id            BIGSERIAL PRIMARY KEY,
			email         TEXT        NOT NULL UNIQUE,
			password_hash TEXT        NOT NULL,
			role          TEXT        NOT NULL DEFAULT 'admin',
			first_name    TEXT        NOT NULL DEFAULT '',
			last_name     TEXT        NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS console.sessions (
			token      TEXT        PRIMARY KEY,
			user_id    BIGINT      NOT NULL REFERENCES console.users(id) ON DELETE CASCADE,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			-- sudo_until: NULL outside sudo mode. Set by /api/auth/sudo when
			-- the user re-authenticates with their password; checked by the
			-- requireSudo middleware on destructive endpoints. Grants a
			-- short window of elevated permission like sudo on a shell, so
			-- a stolen session cookie cannot trigger sign-out-all or
			-- revoke-all without also producing the password.
			sudo_until TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS console_sessions_user_id_idx
			ON console.sessions(user_id);
		CREATE INDEX IF NOT EXISTS console_sessions_expires_idx
			ON console.sessions(expires_at);

		CREATE TABLE IF NOT EXISTS console.caller_repos (
			caller              TEXT        PRIMARY KEY,
			owner               TEXT        NOT NULL,
			repo                TEXT        NOT NULL,
			default_branch      TEXT        NOT NULL DEFAULT 'main',
			schema_path_prefix  TEXT        NOT NULL DEFAULT '',
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);

		-- audit_log is range-partitioned on created_at, one partition per
		-- calendar month, so the retention worker can DROP whole months
		-- atomically (no DELETE-induced bloat). The PK includes
		-- created_at because Postgres requires every unique constraint on
		-- a partitioned table to cover the partition key.
		--
		-- A FK from audit_log to console.users isn't permitted on a
		-- partitioned table that crosses heterogeneous parent rows —
		-- enforce referential integrity at the application layer instead
		-- (users.id is BIGSERIAL and never reused, so a stale user_id in
		-- audit history just shows as user_email='' in the listing).
		CREATE TABLE IF NOT EXISTS console.audit_log (
			id         BIGSERIAL,
			user_id    BIGINT      NOT NULL,
			action     TEXT        NOT NULL,
			detail     JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (id, created_at)
		) PARTITION BY RANGE (created_at);

		CREATE INDEX IF NOT EXISTS console_audit_log_user_id_idx
			ON console.audit_log(user_id);
		CREATE INDEX IF NOT EXISTS console_audit_log_created_at_idx
			ON console.audit_log(created_at DESC);
	`)
	if err != nil {
		return err
	}

	// Ensure the current and next month's partitions exist so the very
	// first logAction call after a cold start lands somewhere. The
	// retention worker keeps this rolling.
	//
	// We pass the first-of-month, not `now`, into the "next" calculation
	// — calling AddDate(0, 1, 0) on a day-31 normalizes through whichever
	// shorter month follows and silently skips a month. (May 31 + 1 month
	// = June 31 → normalized to July 1.)
	now := time.Now().UTC()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := s.ensureAuditPartition(ctx, firstOfMonth); err != nil {
		return fmt.Errorf("ensure current audit partition: %w", err)
	}
	if err := s.ensureAuditPartition(ctx, firstOfMonth.AddDate(0, 1, 0)); err != nil {
		return fmt.Errorf("ensure next audit partition: %w", err)
	}
	return nil
}

// ensureAuditPartition idempotently creates the monthly partition that
// covers `t`'s month. Bounds are [first-of-month, first-of-next-month) so
// rows on the month boundary land in the correct partition.
func (s *store) ensureAuditPartition(ctx context.Context, t time.Time) error {
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	name := fmt.Sprintf("audit_log_p%04d%02d", start.Year(), start.Month())

	// Postgres DDL does not accept $-parameter substitution. We construct
	// `name` and both bounds from a time.Time we built ourselves (never
	// user input), so splicing into the literal SQL is safe. The bounds
	// are rendered as ISO 8601 with explicit UTC offset so Postgres
	// parses them deterministically regardless of session timezone.
	stmt := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS console.%s
		PARTITION OF console.audit_log
		FOR VALUES FROM ('%s') TO ('%s')`,
		name,
		start.Format("2006-01-02 15:04:05-07"),
		end.Format("2006-01-02 15:04:05-07"))
	_, err := s.pool.Exec(ctx, stmt)
	return err
}

// dropAuditPartitionsOlderThan removes every audit_log partition whose
// upper bound is at or before `cutoff`. Idempotent.
//
// Reads pg_partitions metadata to get bounds rather than parsing partition
// names — partition naming is for human eyeballing, not for the worker to
// trust.
func (s *store) dropAuditPartitionsOlderThan(ctx context.Context, cutoff time.Time) (dropped []string, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname,
		       pg_get_expr(c.relpartbound, c.oid) AS bound_expr
		FROM pg_class c
		JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'console'
		  AND c.relispartition = true
		  AND c.relname LIKE 'audit_log_p%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type cand struct{ name, expr string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.name, &c.expr); err != nil {
			return nil, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Bound expr looks like: FOR VALUES FROM ('2026-05-01 00:00:00+00')
	// TO ('2026-06-01 00:00:00+00'). Extract the TO bound.
	for _, c := range cands {
		const marker = "TO ('"
		i := strings.Index(c.expr, marker)
		if i < 0 {
			continue
		}
		rest := c.expr[i+len(marker):]
		j := strings.IndexByte(rest, '\'')
		if j < 0 {
			continue
		}
		upper, err := time.Parse("2006-01-02 15:04:05-07", rest[:j])
		if err != nil {
			// Older PG formats use space-separated TZ — try another shape.
			upper, err = time.Parse("2006-01-02 15:04:05+00", rest[:j])
			if err != nil {
				continue
			}
		}
		if upper.After(cutoff) {
			continue
		}
		if _, err := s.pool.Exec(ctx, fmt.Sprintf("DROP TABLE console.%s", c.name)); err != nil {
			return dropped, fmt.Errorf("drop %s: %w", c.name, err)
		}
		dropped = append(dropped, c.name)
	}
	return dropped, nil
}

// logAction records an operator action in the audit log. Failures are
// non-fatal — the caller receives a log line but the action still succeeds.
func (s *store) logAction(ctx context.Context, userID int64, action string, detail map[string]any) {
	detailJSON, _ := jsonMarshalBytes(detail)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO console.audit_log (user_id, action, detail) VALUES ($1, $2, $3)
	`, userID, action, detailJSON)
}

func (s *store) listAuditLog(ctx context.Context, limit int) ([]auditEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT al.id, al.user_id, u.email, al.action, al.detail, al.created_at
		FROM console.audit_log al
		JOIN console.users u ON u.id = al.user_id
		ORDER BY al.created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.UserID, &e.UserEmail, &e.Action, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}

type auditEntry struct {
	ID        int64
	UserID    int64
	UserEmail string
	Action    string
	Detail    []byte
	CreatedAt time.Time
}

func (s *store) listCallerRepos(ctx context.Context) ([]*CallerRepo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT caller, owner, repo, default_branch, schema_path_prefix
		FROM console.caller_repos ORDER BY caller`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CallerRepo
	for rows.Next() {
		r := &CallerRepo{}
		if err := rows.Scan(&r.Caller, &r.Owner, &r.Repo, &r.DefaultBranch, &r.SchemaPathPrefix); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) getCallerRepo(ctx context.Context, caller string) (*CallerRepo, error) {
	r := &CallerRepo{}
	err := s.pool.QueryRow(ctx, `
		SELECT caller, owner, repo, default_branch, schema_path_prefix
		FROM console.caller_repos WHERE caller = $1`, caller).
		Scan(&r.Caller, &r.Owner, &r.Repo, &r.DefaultBranch, &r.SchemaPathPrefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *store) upsertCallerRepo(ctx context.Context, r *CallerRepo) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO console.caller_repos (caller, owner, repo, default_branch, schema_path_prefix, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (caller) DO UPDATE SET
			owner              = EXCLUDED.owner,
			repo               = EXCLUDED.repo,
			default_branch     = EXCLUDED.default_branch,
			schema_path_prefix = EXCLUDED.schema_path_prefix,
			updated_at         = NOW()
	`, r.Caller, r.Owner, r.Repo, r.DefaultBranch, r.SchemaPathPrefix)
	return err
}

func (s *store) hasAnyUser(ctx context.Context) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM console.users LIMIT 1`).Scan(&n)
	return n > 0, err
}

func (s *store) createUser(ctx context.Context, email, password, role, firstName, lastName string) (*User, error) {
	if role == "" {
		role = "admin"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	var u User
	err = s.pool.QueryRow(ctx, `
		INSERT INTO console.users (email, password_hash, role, first_name, last_name)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, email, role, first_name, last_name, created_at
	`, email, string(hash), role, firstName, lastName).Scan(
		&u.ID, &u.Email, &u.Role, &u.FirstName, &u.LastName, &u.CreatedAt,
	)
	return &u, err
}

func (s *store) listUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, role, first_name, last_name, created_at
		FROM console.users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.FirstName, &u.LastName, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *store) setUserRole(ctx context.Context, userID int64, role string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE console.users SET role = $1 WHERE id = $2
	`, role, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteUser removes a user. The session FK is ON DELETE CASCADE so any
// active session of the deleted user is also gone in the same statement.
// audit_log has no FK (created_at partitioning), so historic actions stay
// readable with the original user_id intact.
func (s *store) deleteUser(ctx context.Context, userID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM console.users WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// changePassword verifies the user's current password then writes a new
// bcrypt hash. Returns ErrNotFound when the current password doesn't
// match (same error as authenticateUser to keep the failure shape
// consistent with login).
func (s *store) changePassword(ctx context.Context, userID int64, currentPassword, newPassword string) error {
	var hash string
	err := s.pool.QueryRow(ctx,
		`SELECT password_hash FROM console.users WHERE id = $1`, userID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(currentPassword)); err != nil {
		return ErrNotFound
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE console.users SET password_hash = $1 WHERE id = $2`,
		string(newHash), userID); err != nil {
		return err
	}
	// Invalidate every session for this user — the user is told to
	// sign in again with the new password.
	_, err = s.pool.Exec(ctx, `DELETE FROM console.sessions WHERE user_id = $1`, userID)
	return err
}

// deleteSessionsForUserExcept signs out every session belonging to
// userID except the one whose token is keepToken. Returns the count of
// sessions removed.
func (s *store) deleteSessionsForUserExcept(ctx context.Context, userID int64, keepToken string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM console.sessions WHERE user_id = $1 AND token <> $2`,
		userID, keepToken)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// deleteAllSessions removes every session in the table. Used by the
// "Sign out all" danger-zone action; the caller is also signed out.
func (s *store) deleteAllSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM console.sessions`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// dummyAuthHash equalises wall-clock time between the wrong-password and
// no-such-user branches of authenticateUser so timing can't enumerate
// valid emails. Comparing against an empty-string hash returns
// ErrHashTooShort instantly and leaks the distinction; comparing against
// a real bcrypt hash forces the same key-stretching work as the
// legitimate path.
//
// Computed once at process start at the same cost real user hashes use
// (bcrypt.DefaultCost). The hashed input is a fixed placeholder, not a
// secret, and the hash output is non-sensitive: bcrypt embeds a random
// salt so even the dummy hash's bytes differ per process.
var dummyAuthHash []byte

func init() {
	h, err := bcrypt.GenerateFromPassword([]byte("invalid-placeholder"), bcrypt.DefaultCost)
	if err != nil {
		// bcrypt at default cost with a short fixed input can't fail under
		// normal conditions; if it does the runtime is broken enough that
		// failing loud at startup is the right move.
		panic(fmt.Sprintf("console: precompute dummy bcrypt hash: %v", err))
	}
	dummyAuthHash = h
}

func (s *store) authenticateUser(ctx context.Context, email, password string) (*User, error) {
	var u User
	var hash string
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, role, password_hash, created_at
		FROM console.users WHERE email = $1
	`, email).Scan(&u.ID, &u.Email, &u.Role, &hash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Burn an actual bcrypt round against a precomputed dummy hash so
		// the no-user wall-clock matches the wrong-password path. Empty
		// `hash` returns ErrHashTooShort instantly and leaks "no such
		// user" via timing.
		_ = bcrypt.CompareHashAndPassword(dummyAuthHash, []byte(password))
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrNotFound
	}
	return &u, nil
}

func (s *store) createSession(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 256 bits of entropy encoded as URL-safe base64 (no padding).
	// 43 chars vs 64 for hex — same entropy, smaller cookie. Existing
	// hex-encoded tokens stay valid because session lookup is a plain
	// string compare; we only emit the new shape going forward.
	token := base64.RawURLEncoding.EncodeToString(b)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO console.sessions (token, user_id, expires_at)
		VALUES ($1, $2, $3)
	`, token, userID, time.Now().Add(sessionTTL))
	return token, err
}

// sessionInfo bundles what middleware needs after a session lookup:
// the user, plus the remaining TTL so it can decide whether to renew.
type sessionInfo struct {
	User      *User
	ExpiresAt time.Time
	SudoUntil *time.Time // nil when not in sudo mode
}

// getSessionInfo returns the session row + the user. Centralises the
// JOIN so the auth middleware and the sudo middleware look at the
// session through one query.
func (s *store) getSessionInfo(ctx context.Context, token string) (*sessionInfo, error) {
	var (
		u         User
		expiresAt time.Time
		sudoUntil *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.role, u.first_name, u.last_name, u.created_at, s.expires_at, s.sudo_until
		FROM console.sessions s
		JOIN console.users u ON u.id = s.user_id
		WHERE s.token = $1 AND s.expires_at > NOW()
	`, token).Scan(&u.ID, &u.Email, &u.Role, &u.FirstName, &u.LastName, &u.CreatedAt, &expiresAt, &sudoUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sessionInfo{User: &u, ExpiresAt: expiresAt, SudoUntil: sudoUntil}, nil
}

// touchSession bumps expires_at to now+sessionTTL. The auth middleware
// only calls this when the session is past the renewal threshold, so
// most authenticated requests don't pay the write cost.
func (s *store) touchSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE console.sessions SET expires_at = $1 WHERE token = $2`,
		time.Now().Add(sessionTTL), token)
	return err
}

// grantSudo elevates the session into sudo mode for sudoTTL. Called by
// handleSudo after the operator re-types their password.
func (s *store) grantSudo(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE console.sessions SET sudo_until = $1 WHERE token = $2`,
		time.Now().Add(sudoTTL), token)
	return err
}

// deleteExpiredSessions is the daily-tick GC for rows whose expires_at
// is already in the past. Functional auth doesn't depend on this — the
// SELECT in getSessionInfo filters them out — but unbounded growth is
// a hygiene problem.
func (s *store) deleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM console.sessions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *store) deleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM console.sessions WHERE token = $1`, token)
	return err
}
