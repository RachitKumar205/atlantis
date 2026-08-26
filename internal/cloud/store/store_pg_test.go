package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Cloud's database, against a real one.
//
// Everything here is env-gated on ATLANTIS_TEST_PG, the same as the console's
// and the server's live-database tests. CI asserts no test skips for that
// reason, so the gate is a local-development convenience.

// newTestStore opens Cloud's database as the role a deployment runs as.
//
// The connecting role decides whether the policies do anything: FORCE ROW LEVEL
// SECURITY binds a table's owner, and binds a superuser to nothing. Connecting
// as the test cluster's superuser would run every test with the boundary inert,
// and every isolation assertion would pass.
//
// So the fixture creates a NOSUPERUSER NOBYPASSRLS role and migrates as it, so
// that role also owns the tables.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, _ := newTestStoreWithDSN(t)
	return db
}

func newTestStoreWithDSN(t *testing.T) (*Store, string) {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise Cloud's database")
	}

	const dbName = "atlantis_cloud_store"
	adminOnDB := pgcatalog.PrivateDatabase(t, adminDSN, dbName)
	dsn := pgcatalog.IsolatedRoleDSN(t, adminOnDB, dbName, "cloud_probe")

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Migrated as the isolated role, so it owns what it creates — which is what
	// FORCE ROW LEVEL SECURITY binds against.
	if err := Migrate(dsn, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	db, err := New(context.Background(), dsn, quiet)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	return db, dsn
}

// An account round-trips, and the id it gets is not derived from anything about
// the person.
func TestCreateAndReadAUser(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "Ada@Example.COM", "Ada", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Folded on write. The database enforces the same rule, so this is checking
	// that the two agree rather than that Go remembered.
	if u.Email != "ada@example.com" {
		t.Errorf("email was stored as %q, not folded", u.Email)
	}
	if u.HasPassword() {
		t.Error("an account created with no password reports having one")
	}
	if !strings.HasPrefix(u.ID, "usr_") {
		t.Errorf("id %q has no recognisable prefix", u.ID)
	}
	// The id becomes the audit actor in every organisation's console, so it
	// must not be the email — an address can be reassigned to another person,
	// which would retroactively change who an audit row says acted.
	if strings.Contains(u.ID, "ada") || strings.Contains(u.ID, "example") {
		t.Errorf("id %q is derived from the email", u.ID)
	}

	got, err := db.UserByEmail(ctx, "ADA@example.com")
	if err != nil {
		t.Fatalf("lookup by a differently-cased address: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("lookup returned %s, want %s", got.ID, u.ID)
	}
}

// One INSERT that skips NormalizeEmail and the same person holds two accounts,
// two sets of memberships and two passwords. The CHECK constraint refuses the
// write, so this goes around the helper.
func TestTheDatabaseRefusesAnUnfoldedEmail(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if _, err := db.CreateUser(ctx, "grace@example.com", "Grace", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := db.pool.Exec(ctx,
		`INSERT INTO cloud.users (id, email) VALUES ($1, $2)`,
		"usr_deadbeef", "Grace@Example.com")
	if err == nil {
		t.Fatal("the database accepted a mixed-case address, so the same person " +
			"can hold two accounts")
	}
	if !strings.Contains(err.Error(), "users_email_check") {
		t.Errorf("refused, but not by the fold constraint: %v", err)
	}

	// And the plain duplicate is refused too, which is the ordinary case the
	// constraint above would mask if it were the only one.
	_, err = db.CreateUser(ctx, "grace@example.com", "Grace Again", nil)
	if err == nil {
		t.Fatal("a duplicate address was accepted")
	}
	// Reported as a named error, so a seeding script can tell "already there"
	// from "the write failed".
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("a duplicate came back as %v, not ErrAlreadyExists", err)
	}
	// The address is in the message, because an operator seeding several
	// accounts needs to know which one.
	if !strings.Contains(err.Error(), "grace@example.com") {
		t.Errorf("the error does not name the address: %v", err)
	}
}

// A user with no memberships reaches no organisation.
func TestMembershipIsTheGate(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "alan@example.com", "Alan", nil)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("create org: %v", err)
	}

	// Before any grant. /authorize consults this.
	if _, err := db.RoleIn(ctx, u.ID, "acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a user with no membership got %v, want ErrNotFound", err)
	}

	if err := db.AddMember(ctx, u.ID, "acme", identity.RoleViewer); err != nil {
		t.Fatalf("add member: %v", err)
	}
	role, err := db.RoleIn(ctx, u.ID, "acme")
	if err != nil {
		t.Fatalf("role after grant: %v", err)
	}
	if role != identity.RoleViewer {
		t.Errorf("role: got %q, want viewer", role)
	}

	// Re-granting changes the role rather than failing or duplicating — an
	// operator promoting someone should not have to remove them first.
	if err := db.AddMember(ctx, u.ID, "acme", identity.RoleAdmin); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	role, _ = db.RoleIn(ctx, u.ID, "acme")
	if role != identity.RoleAdmin {
		t.Errorf("role after re-grant: got %q, want admin", role)
	}

	// And revoking closes the gate again.
	if err := db.RemoveMember(ctx, u.ID, "acme"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := db.RoleIn(ctx, u.ID, "acme"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked membership still answers: %v", err)
	}
	// Revoking twice reports that there was nothing to revoke, so a caller can
	// tell "revoked" from "was never there".
	if err := db.RemoveMember(ctx, u.ID, "acme"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second revoke reported %v, want ErrNotFound", err)
	}
}

// The CHECK constraint duplicates identity.Role. The console refuses a role it
// does not recognise, so a row carrying one authenticates a user who can then
// do nothing. Adding a constant without adding it to the constraint fails here.
func TestEveryRoleTheProductUnderstandsIsStorable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "roles@example.com", "", nil)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.CreateOrg(ctx, "roles", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}

	for _, role := range []identity.Role{identity.RoleAdmin, identity.RoleViewer} {
		if !role.Valid() {
			t.Fatalf("%q is in the list but identity says it is not a role", role)
		}
		if err := db.AddMember(ctx, u.ID, "roles", role); err != nil {
			t.Errorf("the database refused %q, which the product treats as valid: %v", role, err)
		}
	}

	// The counterpart: a role the product does not understand is refused. Going
	// around AddMember's own check, so this exercises the constraint.
	_, err = db.pool.Exec(ctx,
		`INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, 'roles', 'superuser')
		 ON CONFLICT (user_id, org) DO UPDATE SET role = 'superuser'`, u.ID)
	if err == nil {
		t.Error("the database accepted a role the console would refuse, which " +
			"authenticates a user who can then do nothing")
	}
}

func TestOrgNamesAreConstrained(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{
		"Acme",                  // uppercase: the console's RLS discriminator is exact
		"acme corp",             // a space, and this ends up in a URL
		"-acme",                 // leading hyphen is not a DNS label
		"acme_corp",             // underscore is not either
		strings.Repeat("a", 64), // one past a DNS label
		"",
	} {
		if err := db.CreateOrg(ctx, name, ""); err == nil {
			t.Errorf("accepted organisation name %q", name)
		}
	}

	for _, name := range []string{"acme", "acme-corp", "a", "a1", strings.Repeat("a", 63)} {
		if err := db.CreateOrg(ctx, name, ""); err != nil {
			t.Errorf("refused a valid organisation name %q: %v", name, err)
		}
	}
}

// Without the foreign key, `cloud member add -org typo` succeeds, mints
// assertions for an organisation nothing serves, and the failure lands on the
// user at a console that answers 503.
func TestMembershipRequiresARealOrganisation(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "fk@example.com", "", nil)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := db.AddMember(ctx, u.ID, "nosuchorg", identity.RoleAdmin); err == nil {
		t.Fatal("granted membership of an organisation that does not exist")
	}
}

// UserByIdentity runs during a callback, before anyone is signed in, so the
// lookup has to work unbound. Under a per-user RLS policy on cloud.identities
// it would match nothing, and every sign-in would create a second account for
// the same person.
func TestAnOAuthLinkResolvesToItsAccount(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "linked@example.com", "Linked", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "12345", "linked@users.noreply.github.com"); err != nil {
		t.Fatalf("link: %v", err)
	}

	got, err := db.UserByIdentity(ctx, "github", "12345")
	if err != nil {
		t.Fatalf("resolve a linked identity: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("resolved to %s, want %s", got.ID, u.ID)
	}

	if _, err := db.UserByIdentity(ctx, "github", "99999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unlinked provider account got %v, want ErrNotFound", err)
	}

	ids, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("IdentitiesOf: %v, %d links", err, len(ids))
	}
	if ids[0].Provider != "github" || ids[0].Subject != "12345" {
		t.Errorf("link round-tripped as %+v", ids[0])
	}
}

// Without the primary key, a sign-in would have to pick one of two users, and
// whichever it picked would be an account-takeover route for the other.
func TestOneProviderAccountBelongsToOneUser(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	first, err := db.CreateUser(ctx, "first@example.com", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := db.CreateUser(ctx, "second@example.com", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := db.LinkIdentity(ctx, first.ID, "github", "555", ""); err != nil {
		t.Fatalf("first link: %v", err)
	}

	// Straight at the table, around LinkIdentity, so the primary key is what is
	// under test. LinkIdentity's own refusal is asserted in identity_pg_test.go.
	_, err = db.pool.Exec(ctx, `
		INSERT INTO cloud.identities (provider, provider_subject, user_id)
		VALUES ('github', '555', $1)
	`, second.ID)
	if !isUniqueViolation(err) {
		t.Fatalf("a second account claimed one GitHub identity: %v", err)
	}

	owner, err := db.UserByIdentity(ctx, "github", "555")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if owner.ID != first.ID {
		t.Fatalf("the identity resolves to %s, want %s", owner.ID, first.ID)
	}
}

// A membership row surviving its user would grant an organisation to an id
// nothing resolves, and MembersOf would silently drop it on the join — an
// organisation whose member list is shorter than its membership count, with
// nothing reporting why.
func TestDeletingAnAccountRemovesWhatHangsOffIt(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "gone@example.com", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.CreateOrg(ctx, "leaving", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := db.AddMember(ctx, u.ID, "leaving", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "google", "abc", ""); err != nil {
		t.Fatalf("link: %v", err)
	}

	if _, err := db.pool.Exec(ctx, `DELETE FROM cloud.users WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, q := range []string{
		`SELECT count(*) FROM cloud.memberships WHERE user_id = $1`,
		`SELECT count(*) FROM cloud.identities WHERE user_id = $1`,
	} {
		var n int
		if err := db.pool.QueryRow(ctx, q, u.ID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("%d rows outlived the account (%s)", n, q)
		}
	}
}

// Memberships list both ways, which is why the table carries no boundary.
func TestMembershipsListPerUserAndPerOrg(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	for _, org := range []string{"alpha", "beta"} {
		if err := db.CreateOrg(ctx, org, ""); err != nil {
			t.Fatalf("create org: %v", err)
		}
	}
	one, _ := db.CreateUser(ctx, "one@example.com", "", nil)
	two, _ := db.CreateUser(ctx, "two@example.com", "", nil)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	must(db.AddMember(ctx, one.ID, "alpha", identity.RoleAdmin))
	must(db.AddMember(ctx, one.ID, "beta", identity.RoleViewer))
	must(db.AddMember(ctx, two.ID, "alpha", identity.RoleViewer))

	// Per user: this becomes the organisation switcher's list.
	mine, err := db.MembershipsOf(ctx, one.ID)
	if err != nil {
		t.Fatalf("MembershipsOf: %v", err)
	}
	// Ordered by name, so the switcher does not reshuffle between loads.
	if len(mine) != 2 || mine[0].Org != "alpha" || mine[1].Org != "beta" {
		t.Fatalf("MembershipsOf returned %+v, want alpha then beta", mine)
	}
	if mine[0].Role != identity.RoleAdmin || mine[1].Role != identity.RoleViewer {
		t.Errorf("roles came back as %+v", mine)
	}

	// Per org: this is the console's Members panel, and it is the direction a
	// per-user RLS policy would have broken.
	members, err := db.MembersOf(ctx, "alpha")
	if err != nil {
		t.Fatalf("MembersOf: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("alpha has %d members, want 2", len(members))
	}
	if members[0].Email != "one@example.com" || members[1].Email != "two@example.com" {
		t.Errorf("members came back as %+v, want ordered by email", members)
	}
}

// The boot check passes on the schema the migrations produce.
func TestPolicyGuardPassesOnAFreshSchema(t *testing.T) {
	db := newTestStore(t)
	if err := VerifyPolicies(context.Background(), db.pool); err != nil {
		t.Fatalf("the guard refuses the schema its own migrations produce: %v", err)
	}
}

// The fixture table's name is one no migration will add, so a failure here is
// the guard firing rather than a collision with a table that became real.
func TestPolicyGuardRefusesAnUndecidedTable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if _, err := db.pool.Exec(ctx,
		`CREATE TABLE cloud.undecided_fixture (user_id TEXT PRIMARY KEY, secret TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	err := VerifyPolicies(ctx, db.pool)
	if err == nil {
		t.Fatal("a table holding per-user secrets was added with no boundary " +
			"and no decision, and the guard started anyway")
	}
	// On content: the guard has several ways to fail, and a test satisfied by
	// any error would pass with the unknown-table branch deleted.
	if !strings.Contains(err.Error(), "undecided_fixture") {
		t.Errorf("the error does not name the table: %v", err)
	}
	if !strings.Contains(err.Error(), "no row-level security") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
	// And it says where the decision is recorded, because the fix is a list
	// entry rather than a code change.
	if !strings.Contains(err.Error(), "unpolicedTables") {
		t.Errorf("the error does not say where to record the decision: %v", err)
	}
}

// Without FORCE the owning role — which is the role Cloud connects as — reads
// straight through the policy. `\d` lists it, every query returns everything,
// and nothing observable differs from a boundary that works.
func TestPolicyGuardRefusesEnabledButNotForced(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// cloud.current_user_id() already exists — migration 0003 created it for
	// the real policed tables. This adds a table that uses it wrongly.
	for _, sql := range []string{
		`CREATE TABLE cloud.notes (user_id TEXT PRIMARY KEY, body TEXT)`,
		`ALTER TABLE cloud.notes ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY notes_isolation ON cloud.notes AS RESTRICTIVE
		 USING (user_id = cloud.current_user_id())`,
	} {
		if _, err := db.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	err := VerifyPolicies(ctx, db.pool)
	if err == nil {
		t.Fatal("a table with row-level security enabled but not FORCED passed, " +
			"so the owning role reads through the policy and nothing says so")
	}
	if !strings.Contains(err.Error(), "not FORCED") {
		t.Errorf("refused, but not for the missing FORCE: %v", err)
	}
}

func TestPolicyGuardReportsAFailedProbe(t *testing.T) {
	// No database needed: a querier that cannot answer must produce a refusal.
	// A revoked SELECT on pg_policy is what this looks like in a deployment.
	if err := VerifyPolicies(context.Background(), brokenQuerier{}); err == nil {
		t.Fatal("the guard reported success when it could not read the catalogue")
	}
}

type brokenQuerier struct{}

func (brokenQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("permission denied for table pg_policy")
}

func (brokenQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return brokenRow{} }

type brokenRow struct{}

func (brokenRow) Scan(...any) error { return errors.New("permission denied for table pg_roles") }

// The role check runs only once a policed table exists: a role that bypasses
// row-level security bypasses nothing while every table is exempt.
// cloud.totp_secrets and cloud.backup_codes are policed, so whether the
// policies do anything depends on who is connected.
func TestTheRoleCheckHasArmed(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise Cloud's database")
	}
	ctx := context.Background()

	// The isolated role passes: it is NOSUPERUSER NOBYPASSRLS and owns the
	// tables, so the policies apply to it.
	db, _ := newTestStoreWithDSN(t)
	if err := VerifyPolicies(ctx, db.pool); err != nil {
		t.Fatalf("the guard refuses the role a deployment actually runs as: %v", err)
	}

	// The same schema, read by the superuser that created the database, is
	// refused. Without the role check this would pass while every policy in the
	// schema was inert — sign-in working, pages rendering, and every account's
	// second factor readable by any query that forgot a WHERE clause.
	super, err := pgxpool.New(ctx, currentTestDBAdminDSN(t, adminDSN))
	if err != nil {
		t.Fatalf("open as superuser: %v", err)
	}
	defer super.Close()

	err = VerifyPolicies(ctx, super)
	if err == nil {
		t.Fatal("a superuser passed the guard on a schema with policed tables, " +
			"so every policy in it is attached and inert")
	}
	if !strings.Contains(err.Error(), "superuser") {
		t.Errorf("refused, but not because of the role: %v", err)
	}
	// Named, so an operator can see how many tables are affected.
	if !strings.Contains(err.Error(), "2 table(s)") {
		t.Errorf("the error does not say how many tables are affected: %v", err)
	}
}

// currentTestDBAdminDSN points the administrative credentials at the private
// database the fixture just created.
func currentTestDBAdminDSN(t *testing.T, adminDSN string) string {
	t.Helper()
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse %q: %v", adminDSN, err)
	}
	u.Path = "/atlantis_cloud_store"
	return u.String()
}
