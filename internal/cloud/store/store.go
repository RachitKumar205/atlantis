// Package store is Atlantis Cloud's database: who people are, and which
// organisations they belong to.
//
// Every console in the product verifies assertions and holds no accounts of its
// own, so the `org` and `role` claims are only as good as this record. Cloud's
// /authorize checks it before minting.
//
// cloud.users and cloud.identities carry no row-level boundary. They are the
// queries that discover who a request is, so they cannot be filtered by who the
// request is, and a policy on identities would make every OAuth sign-in create a
// duplicate account rather than find the existing one. The tables that can be
// bound are reached through UserStore; policyguard.go refuses to start when a
// table appears in this schema that is neither policed nor explicitly exempt.
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

	"github.com/rachitkumar205/atlantis/internal/analytics"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/migrations"
)

// ErrNotFound reports a row that is not there. Distinct from a failure to
// reach the database, which callers must not treat as "no such user" — that
// would turn a blip into an account that appears not to exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists reports a row that is already there. Named rather than left
// as a driver error, so a re-run of a seeding script can treat it as success.
var ErrAlreadyExists = errors.New("already exists")

// ErrIdentityClaimed reports a provider account that belongs to another user.
//
// Distinct from ErrAlreadyExists, which says only that the row is there.
// Linking is refused rather than moved: a provider account silently changing
// owner is how one person takes another's sign-in method.
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

	// Name is what to call this person, derived by the database from the two
	// fields below. Empty when neither was given, which every caller renders
	// by falling back to Email.
	Name      string
	FirstName string
	LastName  string

	// PasswordHash is nil for an account created by OAuth that has never set
	// one. Such an account may not skip the second factor.
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

	// DisplayName is what the organisation calls itself, empty when it never
	// set one. Org is the slug, which appears in every address and certificate;
	// this is the name a person recognises.
	DisplayName string
}

// Store is Cloud's database handle.
type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// sink receives an event for each audit action that has a projection.
	// Nil is the same as analytics.Discard; see LogAction.
	sink analytics.Sink
}

// UseAnalytics sets where audited actions are reported. Called once at wiring
// time, before the store serves a request.
func (s *Store) UseAnalytics(sink analytics.Sink) { s.sink = sink }

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

// txFinishTimeout bounds the commit and rollback in both transaction helpers,
// which do not use the caller's context. See tx.
const txFinishTimeout = 5 * time.Second

// tx runs fn inside a transaction, for writes that span tables and belong to
// nobody — an organisation with its owner and its queue row, where two of the
// three existing without the third is unrecoverable. UserStore.tx is this with
// an RLS bind in front.
//
// The commit and rollback abandon the caller's context. pgx destroys the pooled
// connection when a rollback Exec fails, so rolling back on an already-cancelled
// context churns the pool. A caller that gives up between the last statement and
// COMMIT would abandon a write that has already happened, and the retry then
// collides with it.
func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), txFinishTimeout)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

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

// newPoolConfig builds the pool's configuration.
//
// Every new physical connection has its user discriminator cleared, against a
// value arriving from outside this process: a role-level default, a SET in the
// connection string's `options`, or a pooler handing back somebody else's
// backend. Any of those pre-binds a user, and the first query that forgot to
// bind reads that user's rows instead of nothing.
//
// SET to the empty string, not RESET. RESET restores the parameter's session
// default, and an `ALTER ROLE ... SET` is that default.
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

// NormalizeEmail is the one place an address is folded for storage and lookup.
//
// A CHECK constraint enforces the same rule, so an INSERT that skipped this
// function is refused rather than quietly creating a second account for the same
// person under different capitalisation.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// CreateUser inserts an account. passwordHash may be nil for an OAuth account.
func (s *Store) CreateUser(ctx context.Context, email, firstName, lastName string, passwordHash *string) (*User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, errors.New("email is required")
	}
	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	firstName, lastName = strings.TrimSpace(firstName), strings.TrimSpace(lastName)
	u := &User{
		ID: id, Email: email,
		FirstName: firstName, LastName: lastName,
		PasswordHash: passwordHash,
	}
	// name is generated from the two columns, so it is returned rather than
	// written.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO cloud.users (id, email, first_name, last_name, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, name
	`, id, email, firstName, lastName, passwordHash).Scan(&u.CreatedAt, &u.Name)
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
		SELECT id, email, name, first_name, last_name,
		       password_hash, email_verified_at, created_at
		FROM cloud.users WHERE email = $1
	`, NormalizeEmail(email))
}

// UserByID looks an account up once it is known.
func (s *Store) UserByID(ctx context.Context, id string) (*User, error) {
	return s.scanUser(ctx, `
		SELECT id, email, name, first_name, last_name,
		       password_hash, email_verified_at, created_at
		FROM cloud.users WHERE id = $1
	`, id)
}

func (s *Store) scanUser(ctx context.Context, sql string, arg any) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, sql, arg).Scan(
		&u.ID, &u.Email, &u.Name, &u.FirstName, &u.LastName,
		&u.PasswordHash, &u.EmailVerifiedAt, &u.CreatedAt)
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
// organisation's console. Not an email: an address can be reassigned to a
// different person, which would retroactively change who an audit row says
// acted. 128 bits from crypto/rand, prefixed so it is recognisable in a log
// line.
func newUserID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate user id: %w", err)
	}
	return "usr_" + hex.EncodeToString(b), nil
}

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
// Distinct from ErrNotFound: the organisation exists and has members, and there
// is nowhere to send them.
var ErrNoConsole = errors.New("no console is registered for this organisation")

// SetConsoleURL records where an organisation's console lives.
//
// Written by `cloud org register`. Also the audience the assertions for that
// console are minted with — see migration 0004 for why those are one value.
//
// The trailing slash is trimmed here rather than at the call site. The value is
// compared against a console's CLOUD_AUDIENCE for exact equality and is
// concatenated with a path to build a redirect, so one stray slash is every
// sign-in for that organisation failing with a message about the token.
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
// The only source of a redirect target: /authorize takes an organisation name,
// checks membership, and asks this, so no destination arrives in the request.
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

// ConsoleURLs returns every registered console URL, once each.
func (s *Store) ConsoleURLs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT console_url FROM cloud.orgs WHERE console_url <> ''`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
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
// The gate /authorize consults before minting. Returns the role rather than a
// boolean because the assertion carries it, read from the same row.
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

// MembershipsOf lists every organisation a user belongs to, ordered by name so
// the console's organisation switcher does not reshuffle between loads.
func (s *Store) MembershipsOf(ctx context.Context, userID string) ([]Membership, error) {
	// LEFT JOIN, not an inner one: a membership whose organisation row has gone
	// still names an organisation, and dropping it here would quietly shorten
	// the switcher rather than showing an entry that cannot be entered.
	rows, err := s.pool.Query(ctx, `
		SELECT m.org, m.role, coalesce(o.display_name, '')
		  FROM cloud.memberships m
		  LEFT JOIN cloud.orgs o ON o.name = m.org
		 WHERE m.user_id = $1
		 ORDER BY m.org`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		var role string
		if err := rows.Scan(&m.Org, &role, &m.DisplayName); err != nil {
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
		SELECT u.id, u.email, u.name, u.first_name, u.last_name,
		       u.password_hash, u.email_verified_at, u.created_at
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
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.FirstName, &u.LastName,
			&u.PasswordHash, &u.EmailVerifiedAt, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

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
// The conflict clause names the user. Without
// `WHERE cloud.identities.user_id = EXCLUDED.user_id`, linking a provider
// account that already belongs to somebody else takes the conflict branch,
// updates provider_email, and returns no error: the caller is told a link was
// made that still points at the other account. With the predicate the update
// matches no row, which is what ErrIdentityClaimed reports.
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
// Removes every identity this user has for that provider. The primary key is
// (provider, provider_subject), so one account can hold two GitHub links, and
// "disconnect GitHub" means both.
//
// An account whose only way in is an OAuth link keeps it. Password reset needs
// a password to reset, so removing the last link locks the account out for
// good, support included.
//
// The rule is in the WHERE clause: between reading that the account also has a
// password and deleting the row, another request can remove that password.
//
// The predicate counts identities belonging to other providers, which is what
// survives the delete. Counting all the user's identities and asking for more
// than one passes an account with two GitHub links and no password, then
// deletes both.
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
	// One or more: the delete covers every link for this provider.
	if tag.RowsAffected() >= 1 {
		return nil
	}

	// Nothing was deleted: either the link was not there, or it was the last way
	// in. Somebody clicking twice and somebody about to be locked out.
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
// One transaction because CreateUser cannot set email_verified_at: three
// separate statements leave two gaps, and a failure in either leaves an account
// that cannot sign in because nothing verified it, or one with no linked
// identity. The retry then hits the UNIQUE constraint on the email.
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

	id, err := newUserID()
	if err != nil {
		return nil, err
	}
	// A provider sends one display string and no division of it. It goes in
	// first_name whole, which renders identically, and splitting it on a space
	// would invent a family name for anyone whose name does not work that way.
	u := &User{ID: id, Email: email, FirstName: strings.TrimSpace(name)}

	err = s.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO cloud.users (id, email, first_name, email_verified_at)
			VALUES ($1, $2, $3, NOW())
			RETURNING created_at, email_verified_at, name
		`, u.ID, u.Email, u.FirstName).Scan(&u.CreatedAt, &u.EmailVerifiedAt, &u.Name)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%s %w", email, ErrAlreadyExists)
			}
			return err
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO cloud.identities (provider, provider_subject, user_id, provider_email)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (provider, provider_subject) DO NOTHING
		`, provider, subject, u.ID, providerEmail)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// The provider account was linked to somebody else between the
			// caller's lookup and here. Rolling back takes the half-made user
			// with it.
			return fmt.Errorf("%s account %s: %w", provider, subject, ErrIdentityClaimed)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
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
// Runs during a callback, before anyone is signed in, which is why
// cloud.identities carries no row-level boundary. Under a policy keyed to the
// current user this would match nothing, and the caller would read that as "not
// linked" and create a second account on every sign-in.
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
