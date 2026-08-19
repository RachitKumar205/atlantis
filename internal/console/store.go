package console

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/secrets"
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

	// Orgs is every organisation this person belongs to, for the switcher.
	//
	// A snapshot like everything else here, and unlike everything else here it
	// is not something to make a decision from. Org above is what this session
	// acts in and what binds every query; this is a list of names to draw. What
	// authorizes a move between them is Cloud's /authorize, which re-reads the
	// membership rather than trusting this.
	Orgs []string
}

type store struct {
	pool *pgxpool.Pool

	// keys seals the per-organisation private keys in console.orgs. See
	// internal/secrets for what that does and does not defend.
	keys secrets.Keyring

	// log exists because audit writes are best-effort and their errors used to
	// be discarded outright. See logAction.
	log *slog.Logger
}

func newStore(ctx context.Context, pgURL string, log *slog.Logger, keys secrets.Keyring) (*store, error) {
	// Built from a config rather than pgxpool.New so every new connection has
	// its organisation discriminator cleared. See newPoolConfig for why that
	// matters and why it is SET rather than RESET.
	cfg, err := newPoolConfig(pgURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &store{pool: pool, log: log, keys: keys}, nil
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
	if _, err := s.pool.Exec(ctx, stmt); err != nil {
		return err
	}

	// A partition inherits NONE of its parent's row-level security.
	//
	// Measured on PostgreSQL 17 rather than assumed: a table created by
	// CREATE TABLE ... PARTITION OF has relrowsecurity false,
	// relforcerowsecurity false, and no policies — and reading it DIRECTLY
	// returned every organisation's rows, bound or unbound, while the parent
	// behaved correctly. The organisation boundary simply is not there.
	//
	// Enabling and forcing RLS with no policy of its own makes the child
	// deny-all on direct access, which is what we want: the console only ever
	// queries the parent, and dropAuditPartitionsOlderThan uses DROP TABLE,
	// which is DDL and outside RLS entirely. So a direct read of a partition is
	// either a bug or an attack, and both deserve nothing.
	//
	// This has to happen here, on every partition, forever — partitions are a
	// function of the calendar, so a migration cannot create next March's.
	// Migration 0004 did it for the partitions that existed when it ran.
	for _, ddl := range []string{
		fmt.Sprintf(`ALTER TABLE console.%s ENABLE ROW LEVEL SECURITY`, name),
		fmt.Sprintf(`ALTER TABLE console.%s FORCE ROW LEVEL SECURITY`, name),
	} {
		if _, err := s.pool.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("isolate partition %s: %w", name, err)
		}
	}
	return nil
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
func (o *orgStore) logAction(ctx context.Context, actor, actorEmail, action string, detail map[string]any) {
	detailJSON, _ := jsonMarshalBytes(detail)
	err := o.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO console.audit_log (actor, actor_email, action, detail, org)
			VALUES ($1, $2, $3, $4, $5)
		`, actor, actorEmail, action, detailJSON, o.org)
		return err
	})
	// Audit writes stay best-effort: an action that succeeded is not undone
	// because recording it failed. What changed is that they can now fail in a
	// new way — a WITH CHECK violation, meaning the bound organisation and the
	// row's organisation disagree — and that is a bug in this file rather than
	// a database being unavailable.
	//
	// This used to discard the error entirely (`_, _ = s.pool.Exec(...)`). An
	// audit log that silently stops recording is the failure this codebase has
	// already been bitten by once, in the TTL sweeper: months of a DELETE that
	// matched nothing looked exactly like months with nothing to delete.
	if err != nil {
		o.db.log.Warn("audit write failed",
			"action", action, "org", o.org, "actor", actor, "err", err)
	}
}

// listAuditLog reads the most recent entries.
//
// It reads what each row carries and joins nothing. The previous version
// INNER JOINed console.users, which meant an action by anyone without a row
// there was not merely unattributed — it was absent from the listing
// entirely. Now that identity comes from Cloud there would have been no such
// row for anybody, so the log would have been silently empty while continuing
// to be written.
// listAuditLog reads the most recent entries for this organisation.
//
// There is no `WHERE org = …` here and that is deliberate: the RESTRICTIVE
// policy on console.audit_log supplies it, so a query written without one
// returns nothing rather than everything. A predicate in the SQL would be a
// second boundary that has to be remembered, and the first thing to go wrong
// would be somebody adding a query and not remembering it — which is exactly
// how this method came to read every organisation's rows in the first place.
func (o *orgStore) listAuditLog(ctx context.Context, limit int) ([]auditEntry, error) {
	var out []auditEntry
	err := o.tx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, actor, actor_email, action, detail, created_at
			FROM console.audit_log
			ORDER BY created_at DESC
			LIMIT $1
		`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var e auditEntry
			var detail []byte
			if err := rows.Scan(&e.ID, &e.Actor, &e.ActorEmail, &e.Action, &detail, &e.CreatedAt); err != nil {
				return err
			}
			e.Detail = detail
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
// subject within this organisation, except the one whose token is keepToken.
//
// Scoped by organisation as well as subject even though Cloud subjects are
// globally unique, so this would be correct without it today. One person can
// belong to several organisations, and "sign out my other devices" should not
// reach into a different organisation's session — that is a different account
// from the user's point of view, whatever the subject says.
func (o *orgStore) deleteSessionsForSubjectExcept(ctx context.Context, subject, keepToken string) (int64, error) {
	tag, err := o.db.pool.Exec(ctx,
		`DELETE FROM console.sessions WHERE org = $1 AND subject = $2 AND token <> $3`,
		o.org, subject, keepToken)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// deleteAllSessions removes every session belonging to this organisation. Used
// by the "Sign out all" danger-zone action; the caller is also signed out.
//
// This was `DELETE FROM console.sessions` with no predicate at all, which was
// correct while one console served one organisation and became a cross-tenant
// action the moment it served several — an admin of one organisation signing
// out every other organisation's users, from a button labelled as affecting
// their own. Nobody decided that; the query simply predated the question.
//
// The organisation comes from the handle rather than an argument precisely so
// that the question cannot be skipped again.
func (o *orgStore) deleteAllSessions(ctx context.Context) (int64, error) {
	tag, err := o.db.pool.Exec(ctx, `DELETE FROM console.sessions WHERE org = $1`, o.org)
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

	// Never nil.
	//
	// The column is NOT NULL and pgx encodes a nil slice as NULL, so an
	// assertion carrying no orgs claim — which is every assertion Cloud minted
	// before the claim existed, and every one it mints when it cannot read
	// memberships — would fail this insert. That is sign-in refused outright to
	// pay for a menu, and the failure is a 500 with nothing in it naming the
	// column.
	orgs := c.Orgs
	if orgs == nil {
		orgs = []string{}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO console.sessions (token, subject, org, role, email, name, orgs, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, token, c.Subject, c.Org, string(c.Role), c.Email, c.Name, orgs,
		time.Now().Add(sessionTTL))
	return token, err
}

// rememberOrg records that an organisation exists.
//
// console.orgs is a registry, not organisation-scoped data, so it carries no
// policy — a table listing organisations that only its own organisation could
// read would be useless.
//
// Written on exchange, where the claims have just been verified, so the only
// way to enter this table is to have presented an assertion Cloud signed for
// that organisation. It writes the name and nothing else: the address and
// credentials arrive later, from `cloud org register`, because an organisation
// is known from its first sign-in and provisioned separately.
func (s *store) rememberOrg(ctx context.Context, org string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO console.orgs (org) VALUES ($1) ON CONFLICT (org) DO NOTHING`, org)
	return err
}

// ErrOrgNotProvisioned reports an organisation with no atlantis behind it.
//
// Distinct from "no such organisation", and both are distinct from a failure to
// reach the database. The console answers 503 for this rather than 404, because
// the organisation is real and somebody signed in to it — what is missing is
// the stack, which is an operator's problem and not the user's mistake.
var ErrOrgNotProvisioned = errors.New("organisation has no atlantis registered")

// orgCredentials is everything needed to reach one organisation's atlantis.
//
// The private key is already decrypted here. It exists in this form only
// between the store and the client that is about to be built from it, which is
// as narrow as the window gets while still being able to dial at all.
type orgCredentials struct {
	Org        string
	Endpoint   string
	HealthAddr string
	CAPEM      string
	CertPEM    string
	KeyPEM     []byte

	// UpdatedAt is what the connection pool compares to decide whether a
	// cached client is still built from the current row. A rotated certificate
	// moves it, and the pool rebuilds without a restart.
	UpdatedAt time.Time
}

// orgCredentials reads one organisation's address and credentials, decrypting
// the private key.
//
// Returns ErrOrgNotProvisioned when the row exists but has not been registered
// — the ordinary state between somebody's first sign-in and an operator
// provisioning their stack — and ErrNotFound when there is no row at all.
func (s *store) orgCredentials(ctx context.Context, org string) (*orgCredentials, error) {
	var (
		c                                    orgCredentials
		endpoint, healthAddr, caPEM, certPEM *string
		keyCT                                []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT atl_endpoint, atl_health_addr, ca_pem, client_cert_pem, client_key_ct, updated_at
		FROM console.orgs WHERE org = $1
	`, org).Scan(&endpoint, &healthAddr, &caPEM, &certPEM, &keyCT, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	// Every column or none. A half-registered row cannot produce a working
	// connection, and reporting it as provisioned would surface later as a
	// confusing TLS or dial error rather than as the missing registration it
	// is.
	if endpoint == nil || healthAddr == nil || caPEM == nil || certPEM == nil || len(keyCT) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrOrgNotProvisioned, org)
	}

	// The organisation name is the associated data the key was sealed with, so
	// a key copied from another row will not open here. See
	// internal/secrets.
	keyPEM, err := s.keys.Decrypt(keyCT, []byte(org))
	if err != nil {
		return nil, fmt.Errorf("decrypt credentials for %s "+
			"(wrong CONSOLE_DATA_KEY, or the row was tampered with): %w", org, err)
	}

	c.Org = org
	c.Endpoint = *endpoint
	c.HealthAddr = *healthAddr
	c.CAPEM = *caPEM
	c.CertPEM = *certPEM
	c.KeyPEM = keyPEM
	return &c, nil
}

// registerOrg stores an organisation's address and credentials, sealing the
// private key.
//
// Used by `cloud org register`. Upserts, so re-registering rotates a
// certificate in place — and touches updated_at, which is how a running console
// notices.
func (s *store) registerOrg(ctx context.Context, c orgCredentials) error {
	// Refuse a registration that cannot dial, here rather than in the exported
	// wrapper, so this is the only door and there is no second one that skips
	// the check. See validateOrgCredentials for what it does and does not
	// claim.
	if err := validateOrgCredentials(c); err != nil {
		return fmt.Errorf("register %s: %w", c.Org, err)
	}
	keyCT, err := s.keys.Encrypt(c.KeyPEM, []byte(c.Org))
	if err != nil {
		return fmt.Errorf("seal the private key for %s: %w", c.Org, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO console.orgs (org, atl_endpoint, atl_health_addr, ca_pem, client_cert_pem, client_key_ct, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (org) DO UPDATE SET
			atl_endpoint    = EXCLUDED.atl_endpoint,
			atl_health_addr = EXCLUDED.atl_health_addr,
			ca_pem          = EXCLUDED.ca_pem,
			client_cert_pem = EXCLUDED.client_cert_pem,
			client_key_ct   = EXCLUDED.client_key_ct,
			updated_at      = NOW()
	`, c.Org, c.Endpoint, c.HealthAddr, c.CAPEM, c.CertPEM, keyCT)
	return err
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
		SELECT subject, org, role, email, name, orgs, expires_at, sudo_until
		FROM console.sessions
		WHERE token = $1 AND expires_at > NOW()
	`, token).Scan(&u.Subject, &u.Org, &u.Role, &u.Email, &u.Name, &u.Orgs,
		&expiresAt, &sudoUntil)
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
