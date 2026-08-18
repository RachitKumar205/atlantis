// Package store is Atlantis Cloud's database: who people are, and which
// organisations they belong to.
//
// # What this is the record of
//
// Every console in the product verifies assertions and holds no accounts of its
// own. Until this existed, the `org` claim in those assertions was whatever
// `cloud mint -org` was handed, and the `role` claim likewise — Cloud asserted
// membership it had no record of. This is that record, and Cloud's /authorize
// checks it before minting.
//
// # Why there is no bound handle here yet
//
// The console has orgStore, a handle that makes an unscoped query on a policed
// table a compile error. Cloud gets the equivalent when it has a table that can
// carry a boundary — none of the four here can, for reasons set out in
// migrations/cloud/0001. The short version: users and identities are the
// queries that *discover* who a request is, so they cannot be filtered by who
// the request is, and a policy on identities would make every OAuth sign-in
// create a duplicate account rather than find the existing one.
//
// What does exist from today is policyguard.go, which refuses to start if a
// table appears in this schema that is neither policed nor explicitly exempt.
// That is what stops the second-factor secrets landing unpoliced without anyone
// deciding.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/migrations"
)

// ErrNotFound reports a row that is not there. Distinct from a failure to
// reach the database, which callers must not treat as "no such user" — that
// would turn a blip into an account that appears not to exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists reports a row that is already there.
//
// Named rather than left as a raw SQLSTATE so a caller can decide. Creating an
// account that exists is not a failure for a seeding script and is worth
// reporting to an operator, and those want different handling — which is
// impossible if the only signal is a constraint name in a driver error.
var ErrAlreadyExists = errors.New("already exists")

// ErrIdentityClaimed reports a provider account that belongs to another user.
//
// Distinct from ErrAlreadyExists, which says the row is there. This says it is
// there and points somewhere else — the difference between "you already linked
// this" and "somebody else did". Linking is refused rather than moved: a
// provider account silently changing owner is how one person takes another's
// sign-in method.
var ErrIdentityClaimed = errors.New("already linked to another account")

// ErrLastSignInMethod reports an unlink that would lock the account out.
//
// An account with no password and one identity has exactly one way in. Removing
// it leaves nobody able to reach the account, support included, because the
// reset flow needs a password to reset.
var ErrLastSignInMethod = errors.New("this is the only way to sign in to this account")

// uniqueViolation is PostgreSQL's SQLSTATE for a duplicate key.
//
// Matched on the code rather than the message: the message names the
// constraint, which is an implementation detail of the migration and changes
// when a column is renamed.
const uniqueViolation = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// User is a Cloud account.
type User struct {
	ID    string
	Email string
	Name  string

	// PasswordHash is nil for an account created by OAuth that has never set
	// one. That is a supported state, not an incomplete one — see the
	// migration. What such an account may not do is skip the second factor.
	PasswordHash *string

	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// HasPassword reports whether this account can be signed in to with one.
func (u *User) HasPassword() bool { return u.PasswordHash != nil && *u.PasswordHash != "" }

// Membership is one user's place in one organisation.
type Membership struct {
	Org  string
	Role identity.Role
}

// Store is Cloud's database handle.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New opens the pool and verifies it answers.
func New(ctx context.Context, pgURL string, log *slog.Logger) (*Store, error) {
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
	return &Store{pool: pool, log: log}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool for the boot-time policy check, which has to
// ask the live catalogue rather than the code.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// newPoolConfig builds the pool's configuration.
//
// Every new physical connection has its user discriminator cleared. Cloud's own
// binds are transaction-local and revert by themselves, so this is not about
// them — it covers a value arriving from outside this process: a role-level
// default, a SET in the connection string's `options`, or a pooler handing back
// somebody else's backend. Any of those would pre-bind a user, and the first
// query that forgot to bind would read that user's rows instead of nothing.
//
// SET to the empty string, deliberately NOT RESET. RESET restores the
// parameter's session default, and an `ALTER ROLE ... SET` *is* that default —
// so RESET would restore precisely the value it is meant to clear. The console
// carries the same note; a test caught it on the server side first.
func newPoolConfig(pgURL string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(pgURL)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, `SET "cloud.user" = ''`); err != nil {
			return fmt.Errorf("clear user discriminator: %w", err)
		}
		return nil
	}
	return cfg, nil
}

// Migrate applies Cloud's schema from the tree embedded in this binary.
func Migrate(pgURL string, log *slog.Logger) error {
	return migrate.RunFS(pgURL, migrations.Cloud, "cloud", migrate.CloudHistoryTable, log)
}

// ── Users ───────────────────────────────────────────────────────────────────

// NormalizeEmail is the one place an address is folded for storage and lookup.
//
// The database enforces the same rule with a CHECK constraint, which is what
// makes this a convenience rather than the mechanism. An INSERT that skipped
// this function would be refused rather than quietly creating a second account
// for the same person under different capitalisation.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// CreateUser inserts an account. passwordHash may be nil for an OAuth account.
func (s *Store) CreateUser(ctx context.Context, email, name string, passwordHash *string) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	u := &User{ID: id, Email: email, Name: name, PasswordHash: passwordHash}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO cloud.users (id, email, name, password_hash)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at
	`, id, email, name, passwordHash).Scan(&u.CreatedAt)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%s %w", email, ErrAlreadyExists)
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UserByEmail looks an account up for sign-in.
//
// Unscoped by necessity: this is the query that discovers who is asking, so
// there is nothing to bind yet. See the package comment.
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return s.scanUser(ctx, `
		SELECT id, email, name, password_hash, email_verified_at, created_at
		FROM cloud.users WHERE email = $1
	`, NormalizeEmail(email))
}

// UserByID looks an account up once it is known.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return s.scanUser(ctx, `
		SELECT id, email, name, password_hash, email_verified_at, created_at
		FROM cloud.users WHERE id = $1
	`, id)
}

func (s *Store) scanUser(ctx context.Context, sql string, arg any) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, sql, arg).Scan(
		&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.EmailVerifiedAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// newUserID mints an opaque, stable identifier.
//
// This becomes the `sub` claim and therefore the audit actor in every
// organisation's console. It is deliberately not an email: an address can be
// reassigned to a different person, which would retroactively change who an
// audit row says acted. 128 bits from crypto/rand, prefixed so it is
// recognisable in a log line.
func newUserID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate user id: %w", err)
	}
	return "usr_" + hex.EncodeToString(b), nil
}

// ── Organisations and membership ────────────────────────────────────────────

// CreateOrg records an organisation. Idempotent: re-creating an existing one is
// not an error, because `cloud org register` is expected to be re-runnable.
func (s *Store) CreateOrg(ctx context.Context, name, displayName string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cloud.orgs (name, display_name) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET display_name = EXCLUDED.display_name
	`, name, displayName)
	return err
}

// ErrNoConsole reports an organisation nobody has told Cloud how to reach.
//
// Distinct from ErrNotFound, which says there is no such organisation. This one
// exists and has members; there is simply nowhere to send them. The two need
// telling apart because they are different mistakes — a typo in a name, versus
// a provisioning step nobody ran.
var ErrNoConsole = errors.New("no console is registered for this organisation")

// SetConsoleURL records where an organisation's console lives.
//
// Written by `cloud org register`. Also the audience the assertions for that
// console are minted with — see migration 0004 for why those are one value.
//
// The trailing slash is trimmed HERE rather than at the call site. This value
// is compared against a console's CLOUD_AUDIENCE for exact equality and is
// concatenated with a path to build a redirect, so one stray slash is every
// sign-in for that organisation failing with a message about the token. Doing
// it in the command that happens to write it today would leave the next writer
// to remember, and the failure it produces gives no hint what to remember.
func (s *Store) SetConsoleURL(ctx context.Context, org, consoleURL string) error {
	consoleURL = strings.TrimRight(strings.TrimSpace(consoleURL), "/")
	tag, err := s.pool.Exec(ctx,
		`UPDATE cloud.orgs SET console_url = $2 WHERE name = $1`, org, consoleURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	return nil
}

// ConsoleURL returns where to send somebody who has been authorized into an
// organisation.
//
// # Why this is the only source of a redirect target
//
// /authorize takes no destination from the request. It takes an organisation
// name, checks membership, and asks this. A URL that arrived in a query
// parameter would need validating against something, and the something would
// be this column anyway — so the parameter is skipped and the column consulted
// directly. There is then no input that could be validated wrongly.
//
// Reports ErrNoConsole when the organisation exists but has no console, which
// is a state `cloud org create` leaves behind and `cloud org register` clears.
func (s *Store) ConsoleURL(ctx context.Context, org string) (string, error) {
	var consoleURL string
	err := s.pool.QueryRow(ctx,
		`SELECT console_url FROM cloud.orgs WHERE name = $1`, org).Scan(&consoleURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%s: %w", org, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if consoleURL == "" {
		return "", fmt.Errorf("%s: %w", org, ErrNoConsole)
	}
	return consoleURL, nil
}

// AddMember grants a user a role in an organisation, replacing any existing
// grant. The role is validated here as well as by the database, so a caller
// gets a message about the role rather than a constraint name.
func (s *Store) AddMember(ctx context.Context, userID, org string, role identity.Role) error {
	if !role.Valid() {
		return fmt.Errorf("%q is not a role this product understands (admin, viewer)", role)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, org) DO UPDATE SET role = EXCLUDED.role
	`, userID, org, string(role))
	return err
}

// RemoveMember revokes a grant. Reports ErrNotFound if there was none, so a
// caller can tell "revoked" from "was never there".
func (s *Store) RemoveMember(ctx context.Context, userID, org string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM cloud.memberships WHERE user_id = $1 AND org = $2`, userID, org)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RoleIn returns the user's role in one organisation, or ErrNotFound.
//
// This is the gate /authorize consults before minting. It returns the role
// rather than a boolean because the assertion carries it, and reading both from
// one row is what stops the two disagreeing.
func (s *Store) RoleIn(ctx context.Context, userID, org string) (identity.Role, error) {
	var role string
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM cloud.memberships WHERE user_id = $1 AND org = $2`,
		userID, org).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return identity.Role(role), nil
}

// MembershipsOf lists every organisation a user belongs to, ordered by name.
//
// Ordered so the console's organisation switcher is stable between loads —
// an unordered list reshuffles on every query and the entry under the cursor
// moves.
func (s *Store) MembershipsOf(ctx context.Context, userID string) ([]Membership, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT org, role FROM cloud.memberships WHERE user_id = $1 ORDER BY org`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		var role string
		if err := rows.Scan(&m.Org, &role); err != nil {
			return nil, err
		}
		m.Role = identity.Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MembersOf lists an organisation's members. Read per-org rather than per-user,
// which is why memberships carries no row-level boundary — see the package
// comment.
func (s *Store) MembersOf(ctx context.Context, org string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, u.password_hash, u.email_verified_at, u.created_at
		FROM cloud.memberships m
		JOIN cloud.users u ON u.id = m.user_id
		WHERE m.org = $1
		ORDER BY u.email
	`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash,
			&u.EmailVerifiedAt, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ── OAuth identities ────────────────────────────────────────────────────────

// Identity is a linked OAuth account.
type Identity struct {
	Provider  string
	Subject   string
	Email     string
	CreatedAt time.Time
}

// LinkIdentity records an OAuth account against a user.
//
// Idempotent for the user who already owns the link — re-linking refreshes the
// display address and reports success, which is what a second sign-in through
// the same provider does.
//
// # Why the conflict clause names the user
//
// Without `WHERE cloud.identities.user_id = EXCLUDED.user_id`, an attempt to
// link a provider account that ALREADY BELONGS TO SOMEBODY ELSE takes the
// conflict branch, updates only provider_email, and returns no error. The
// caller is told the link was made. It was not, and it still points at the
// other account.
//
// That is the shape this package keeps paying for: a write that succeeds while
// changing nothing it claimed to change. With the predicate, the update matches
// no row and the row count says so, which is what ErrIdentityClaimed reports.
func (s *Store) LinkIdentity(ctx context.Context, userID, provider, subject, email string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO cloud.identities (provider, provider_subject, user_id, provider_email)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, provider_subject) DO UPDATE
		   SET provider_email = EXCLUDED.provider_email
		 WHERE cloud.identities.user_id = EXCLUDED.user_id
	`, provider, subject, userID, email)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s account %s: %w", provider, subject, ErrIdentityClaimed)
	}
	return nil
}

// UnlinkIdentity disconnects a provider from an account.
//
// Removes EVERY identity this user has for that provider, because "disconnect
// GitHub" is the thing a person means and one account can hold two links from
// one provider — the primary key is (provider, provider_subject), so linking a
// second GitHub account is allowed.
//
// # Why the rule lives in the WHERE clause
//
// An account whose only way in is an OAuth link must not be able to remove it.
// There is no recovery from that state: password reset needs a password to
// reset, so the account is reachable by nobody, support included.
//
// The condition is part of the DELETE rather than a check the handler performs
// first. Two reasons, and the second is the one that matters. A separate check
// is one somebody can forget at the next call site — and between reading "this
// account also has a password" and deleting the row, another request can remove
// that password. One statement has no such window: Postgres evaluates the
// predicate against the same snapshot it deletes from.
//
// # What the predicate counts, and the version of it that was wrong
//
// It counts identities belonging to OTHER providers. The first draft counted
// all of the user's identities and asked for more than one, which passes for an
// account with two GitHub links and no password — and then deletes both,
// leaving exactly the locked-out account this function exists to prevent. The
// count has to describe what survives the delete, not what exists before it.
func (s *Store) UnlinkIdentity(ctx context.Context, userID, provider string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM cloud.identities
		 WHERE user_id = $1 AND provider = $2
		   AND (EXISTS (SELECT 1 FROM cloud.users
		                 WHERE id = $1
		                   AND password_hash IS NOT NULL AND password_hash <> '')
		        OR EXISTS (SELECT 1 FROM cloud.identities
		                    WHERE user_id = $1 AND provider <> $2))
	`, userID, provider)
	if err != nil {
		return err
	}
	// One or more: the delete covers every link for this provider, and reporting
	// "not found" after removing two of them would be the same shape of lie
	// this file keeps closing.
	if tag.RowsAffected() >= 1 {
		return nil
	}

	// Nothing was deleted, and the two reasons need telling apart: the link was
	// not there, or it was the last way in. Reported distinctly because one is
	// somebody clicking twice and the other is somebody about to be locked out.
	var linked bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM cloud.identities WHERE user_id = $1 AND provider = $2)`,
		userID, provider).Scan(&linked); err != nil {
		return err
	}
	if !linked {
		return ErrNotFound
	}
	return ErrLastSignInMethod
}

// CreateUserWithIdentity creates an OAuth account and its link together.
//
// # Why this is not CreateUser followed by two more calls
//
// An account arriving through a provider needs three things written: the user,
// the fact that the address is already verified, and the identity. CreateUser
// cannot set email_verified_at, so done separately that is three statements
// with two gaps in it, and a failure in either gap leaves a row somebody is
// stuck behind — an account that cannot sign in because nothing verified it, or
// one with no way in because the link never landed. Neither is visible as an
// error to the person who just consented; both present as "it didn't work",
// and the retry hits the UNIQUE constraint on the email.
//
// The address is stored verified because the provider attested it, and the
// caller has already refused anything the provider did not flag as verified.
func (s *Store) CreateUserWithIdentity(
	ctx context.Context, email, name, provider, subject, providerEmail string,
) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, errors.New("email is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Bounded, and on a context the request cannot cancel — the same
		// discipline as UserStore.tx, for the same reason: pgx destroys the
		// pooled connection when a rollback Exec fails, and a browser that
		// closed mid-callback is the common case rather than an exotic one.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	u := &User{ID: id, Email: email, Name: name}
	err = tx.QueryRow(ctx, `
		INSERT INTO cloud.users (id, email, name, email_verified_at)
		VALUES ($1, $2, $3, NOW())
		RETURNING created_at, email_verified_at
	`, u.ID, u.Email, u.Name).Scan(&u.CreatedAt, &u.EmailVerifiedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%s %w", email, ErrAlreadyExists)
		}
		return nil, err
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO cloud.identities (provider, provider_subject, user_id, provider_email)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider, provider_subject) DO NOTHING
	`, provider, subject, u.ID, providerEmail)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// The provider account was linked to somebody else between the caller's
		// lookup and here. Rolling back takes the half-made user with it.
		return nil, fmt.Errorf("%s account %s: %w", provider, subject, ErrIdentityClaimed)
	}

	// Committed on a context the request cannot cancel, so a browser that gives
	// up mid-callback does not abort a write that has already happened —
	// leaving an account the retry then collides with on the UNIQUE email.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bindTimeout)
	defer cancel()
	if err := tx.Commit(cctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return u, nil
}

// IdentitiesOf lists a user's linked accounts.
func (s *Store) IdentitiesOf(ctx context.Context, userID string) ([]Identity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT provider, provider_subject, provider_email, created_at
		FROM cloud.identities WHERE user_id = $1 ORDER BY provider, created_at
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Provider, &i.Subject, &i.Email, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UserByIdentity resolves an OAuth account to a Cloud user.
//
// This runs during a callback, before anyone is signed in, and it is the query
// that establishes who they are — which is exactly why cloud.identities carries
// no row-level boundary. Under a policy keyed to the current user this would
// match nothing, and the caller would read that as "not linked" and create a
// second account for the same person on every sign-in.
func (s *Store) UserByIdentity(ctx context.Context, provider, subject string) (*User, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM cloud.identities WHERE provider = $1 AND provider_subject = $2`,
		provider, subject).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.UserByID(ctx, userID)
}
