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

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
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

// ErrNotFound is returned when a session is not found or has expired.
var ErrNotFound = errors.New("not found")

// ErrAssertionSpent reports an assertion that has already been exchanged.
//
// It is deliberately distinct from a verification failure. The assertion is
// genuine and unexpired; what is wrong is that it is being presented a second
// time, which is either a replay or a page that submitted twice.
var ErrAssertionSpent = errors.New("assertion has already been used")

// User is who is making the current request.
//
// Every field arrives in a signed assertion from Cloud and is copied onto the
// session at sign-in. The console stores no user record, so there is nothing
// here to look up and nothing to keep in step — the values are a snapshot of
// what Cloud asserted, fixed for the life of the session. A role change at
// Cloud therefore takes effect at the user's next sign-in.
type User struct {
	// Subject is Cloud's stable identifier, and the audit actor. Not an
	// email: an address can be reassigned to a different person, which would
	// retroactively change who an audit row says acted.
	Subject string

	// Org is the organisation this session is acting in.
	Org string

	Role  string // "admin" | "viewer"
	Email string
	Name  string
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

// The console's schema lives in migrations/console, applied by internal/migrate
// from the tree embedded in this binary.
//
// It used to be built here, by one idempotent CREATE-IF-NOT-EXISTS block re-run
// on every boot, with one-shot DROP statements appended as features were
// removed. That has no version, so nothing could distinguish "already applied"
// from "applied halfway" — survivable while every statement was CREATE TABLE,
// and not once one of them is an ALTER or a policy.

// ensureAuditPartitions creates the audit_log partitions the console needs to
// start: this month's and next month's.
//
// Separate from the migration tree on purpose. audit_log is partitioned by
// month, so its children are a function of the calendar rather than of the
// schema version — a static migration written today cannot create next March's
// partition. The migration creates the partitioned parent; this creates the
// children, and the retention worker keeps the window rolling.
func (s *store) ensureAuditPartitions(ctx context.Context) error {
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
// logAction records an operator action.
//
// The actor's email is written onto the row rather than resolved when the log
// is read. An audit entry should say who acted at the time it happened; a
// lookup would report whoever holds that identity now, which is a different
// claim and occasionally a false one.
func (s *store) logAction(ctx context.Context, actor, actorEmail, action string, detail map[string]any) {
	detailJSON, _ := jsonMarshalBytes(detail)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO console.audit_log (actor, actor_email, action, detail) VALUES ($1, $2, $3, $4)
	`, actor, actorEmail, action, detailJSON)
}

// listAuditLog reads the most recent entries.
//
// It reads what each row carries and joins nothing. The previous version
// INNER JOINed console.users, which meant an action by anyone without a row
// there was not merely unattributed — it was absent from the listing
// entirely. Now that identity comes from Cloud there would have been no such
// row for anybody, so the log would have been silently empty while continuing
// to be written.
func (s *store) listAuditLog(ctx context.Context, limit int) ([]auditEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, actor, actor_email, action, detail, created_at
		FROM console.audit_log
		ORDER BY created_at DESC
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
		if err := rows.Scan(&e.ID, &e.Actor, &e.ActorEmail, &e.Action, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}

type auditEntry struct {
	ID         int64
	Actor      string // Cloud subject, or "local:N" for actions predating migration 0003
	ActorEmail string
	Action     string
	Detail     []byte
	CreatedAt  time.Time
}

// deleteSessionsForSubjectExcept signs out every session belonging to a Cloud
// subject except the one whose token is keepToken. Returns how many were
// removed.
func (s *store) deleteSessionsForSubjectExcept(ctx context.Context, subject, keepToken string) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM console.sessions WHERE subject = $1 AND token <> $2`,
		subject, keepToken)
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

// createSession opens a session from a verified assertion.
//
// The claims are copied onto the row. There is no user table to point at, and
// the session is meant to be a snapshot of what Cloud asserted at sign-in
// rather than a live view of it.
func (s *store) createSession(ctx context.Context, c *identity.Claims) (string, error) {
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
		INSERT INTO console.sessions (token, subject, org, role, email, name, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, token, c.Subject, c.Org, string(c.Role), c.Email, c.Name, time.Now().Add(sessionTTL))
	return token, err
}

// spendAssertion records an assertion id, and reports ErrAssertionSpent if it
// was already there.
//
// INSERT ... ON CONFLICT DO NOTHING makes the check and the claim one
// statement. Reading first and inserting second would leave a window in which
// two concurrent requests both see the id as unused, which is precisely the
// race a replay would exploit — and the narrower it gets the harder the
// resulting bug is to believe.
func (s *store) spendAssertion(ctx context.Context, jti string, expiresAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO console.spent_assertions (jti, expires_at)
		VALUES ($1, $2)
		ON CONFLICT (jti) DO NOTHING
	`, jti, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAssertionSpent
	}
	return nil
}

// deleteSpentAssertions drops records of assertions that have since expired.
//
// Safe to forget them at that point: an expired assertion is refused on its
// own exp before the replay check is ever reached, so the row has stopped
// carrying information. Without this the table grows once per sign-in forever.
func (s *store) deleteSpentAssertions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM console.spent_assertions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// sessionInfo bundles what middleware needs after a session lookup:
// the user, plus the remaining TTL so it can decide whether to renew.
type sessionInfo struct {
	User      *User
	ExpiresAt time.Time
	SudoUntil *time.Time // nil when not in sudo mode
}

// getSessionInfo returns the session row, which now carries the identity
// itself. One query, no join — the row is self-contained.
func (s *store) getSessionInfo(ctx context.Context, token string) (*sessionInfo, error) {
	var (
		u         User
		expiresAt time.Time
		sudoUntil *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT subject, org, role, email, name, expires_at, sudo_until
		FROM console.sessions
		WHERE token = $1 AND expires_at > NOW()
	`, token).Scan(&u.Subject, &u.Org, &u.Role, &u.Email, &u.Name, &expiresAt, &sudoUntil)
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
