package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Cloud's database, against a real one.
//
// Everything here is env-gated on ATLANTIS_TEST_PG, the same as the console's
// and the server's live-database tests. CI asserts no test skips for that
// reason, so the gate is a local-development convenience rather than a way for
// these to be quietly absent.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise Cloud's database")
	}

	dsn := pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_cloud_store")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Migrate(dsn, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	db, err := New(context.Background(), dsn, quiet)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
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

// The database refuses a second account for the same person under different
// capitalisation, whatever Go did.
//
// This is the half that matters. Normalising in NormalizeEmail is uniqueness
// enforced by remembering; one INSERT that skips the helper and the same person
// has two accounts, two sets of memberships and two passwords. The CHECK moves
// that from a bug to a refused write, so the test goes around the helper
// deliberately.
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
	// Reported as a named error rather than a raw constraint violation. A
	// seeding script needs to tell "already there" from "the write failed", and
	// the alternative it reaches for is ignoring every error from the command —
	// which also ignores the ones that matter.
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

	// Before any grant. This is what /authorize consults, and the answer has to
	// be a refusal rather than a default.
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

// Every role the product understands is a role the database accepts.
//
// The CHECK constraint duplicates identity.Role, which is deliberate — the
// console refuses a role it does not recognise, so a row carrying one would
// authenticate a user who could then do nothing. This is what stops the two
// drifting: add a constant without adding it to the constraint and this fails.
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

// Organisation names are constrained where they are stored, because of
// everywhere they travel afterwards.
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

// A membership cannot name an organisation that does not exist.
//
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

// An OAuth link resolves to its account, and the lookup works unbound.
//
// The unbound part is the point. UserByIdentity runs during a callback, before
// anyone is signed in — it is the query that establishes who they are. An
// earlier draft of this schema put a per-user RLS policy on cloud.identities,
// under which this would have matched nothing and every sign-in would have
// created a second account for the same person, silently.
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

// One provider account cannot be claimed by two Cloud accounts.
//
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
	// The upsert re-points the link rather than creating a second row, so the
	// property to assert is that exactly one user owns it — not that the second
	// link fails.
	if err := db.LinkIdentity(ctx, second.ID, "github", "555", ""); err != nil {
		t.Fatalf("second link: %v", err)
	}

	var n int
	if err := db.pool.QueryRow(ctx,
		`SELECT count(*) FROM cloud.identities WHERE provider = 'github' AND provider_subject = '555'`).
		Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("one provider account is claimed by %d rows", n)
	}
}

// Deleting an account takes its memberships and links with it.
//
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
	if len(mine) != 2 || mine[0].Org != "alpha" || mine[1].Org != "beta" {
		t.Fatalf("MembershipsOf returned %+v, want alpha then beta", mine)
	}
	// Ordered, so the switcher does not reshuffle between loads and move the
	// entry under the cursor.
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

// A new table with no boundary and no recorded decision fails the boot.
//
// This is the whole point of the guard. Every table Cloud has today is exempt,
// so nothing it currently checks can come out false — what it is for is the
// next one. The second-factor secrets are the first tables here that are only
// ever read for an already-identified user, and this is what stops them landing
// unpoliced because nobody thought about it.
func TestPolicyGuardRefusesAnUndecidedTable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if _, err := db.pool.Exec(ctx,
		`CREATE TABLE cloud.totp_secrets (user_id TEXT PRIMARY KEY, secret TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	err := VerifyPolicies(ctx, db.pool)
	if err == nil {
		t.Fatal("a table holding every user's second factor was added with no " +
			"boundary and no decision, and the guard started anyway")
	}
	// On content: the guard has several ways to fail, and a test satisfied by
	// any error would pass with the unknown-table branch deleted.
	if !strings.Contains(err.Error(), "totp_secrets") {
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

// ENABLE without FORCE is the trap, and the guard catches it.
//
// Without FORCE the owning role — which is the role Cloud connects as — reads
// straight through the policy. `\d` lists it, every query returns everything,
// and nothing observable differs from a boundary that works.
func TestPolicyGuardRefusesEnabledButNotForced(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	for _, sql := range []string{
		`CREATE TABLE cloud.notes (user_id TEXT PRIMARY KEY, body TEXT)`,
		`CREATE FUNCTION cloud.current_user_id() RETURNS text LANGUAGE sql STABLE AS
		 $$ SELECT nullif(pg_catalog.current_setting('cloud.user', true), '') $$`,
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

// A check that could not run is not a check that passed.
func TestPolicyGuardReportsAFailedProbe(t *testing.T) {
	// No database needed: the point is that a querier which cannot answer
	// produces a refusal rather than a pass. A revoked SELECT on pg_policy is
	// what this looks like in a deployment.
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

// The role check arms itself when a policed table appears, and not before.
//
// This is the half that is easy to get wrong in the safe direction: a guard
// that never asks about the role passes every test that only creates exempt
// tables, and then a superuser reads through the first real policy. The two
// halves are asserted separately here because they fail for different reasons.
func TestTheRoleCheckArmsWithTheFirstPolicedTable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	// The test role is the administrative one, which is a superuser — so if the
	// role check ran unconditionally, this would already be failing.
	if err := VerifyPolicies(ctx, db.pool); err != nil {
		t.Fatalf("the guard refused a schema where every table is exempt, so it "+
			"is asking about the role before there is anything to protect: %v", err)
	}

	// Now give it something to protect.
	for _, sql := range []string{
		`CREATE FUNCTION cloud.current_user_id() RETURNS text LANGUAGE sql STABLE AS
		 $$ SELECT nullif(pg_catalog.current_setting('cloud.user', true), '') $$`,
		`CREATE TABLE cloud.totp_secrets (user_id TEXT PRIMARY KEY, secret TEXT NOT NULL)`,
		`ALTER TABLE cloud.totp_secrets ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE cloud.totp_secrets FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY totp_isolation ON cloud.totp_secrets AS RESTRICTIVE
		 USING (user_id = cloud.current_user_id())`,
	} {
		if _, err := db.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("setup %q: %v", sql, err)
		}
	}

	err := VerifyPolicies(ctx, db.pool)
	if err == nil {
		t.Fatal("a policed table exists and the connecting role is a superuser, " +
			"which reads through every policy in the schema — and the guard " +
			"passed anyway")
	}
	if !strings.Contains(err.Error(), "superuser") {
		t.Errorf("refused, but not because of the role: %v", err)
	}
	// Named, because the operator's next question is which table made this
	// start mattering.
	if !strings.Contains(err.Error(), "1 table(s)") {
		t.Errorf("the error does not say how much is at stake: %v", err)
	}
}
