package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// Creating an organisation the way a browser will, and the ways it must refuse.

// Posting a name somebody else owns must not hand you their organisation.
//
// This is the whole reason CreateOrgForOwner exists rather than a flag on
// CreateOrgWithOwner. That function upserts — deliberately, because `cloud org
// create` is documented as safe to re-run — and its memberships upsert is
// `ON CONFLICT (user_id, org) DO UPDATE SET role`. For a user with no existing
// row that is a plain INSERT, so posting somebody else's organisation name
// makes you its admin and rewrites its display name, and nothing refuses.
func TestCreatingATakenNameGrantsNothing(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	alice := ownerFor(t, db, "alice-taken@example.com")
	bob := ownerFor(t, db, "bob-taken@example.com")

	if err := db.CreateOrgForOwner(ctx, "taken-org", "Alice's Company", alice, identity.RoleAdmin); err != nil {
		t.Fatalf("alice could not create: %v", err)
	}

	err := db.CreateOrgForOwner(ctx, "taken-org", "Bob's Company", bob, identity.RoleAdmin)

	// Errorf, not Fatalf, and the ordering is deliberate. The two assertions
	// below are the ones that matter — whether Bob got anything — and they must
	// run even when the refusal arrives in an unexpected shape. A mutation that
	// made this upsert like the CLI version was caught here by a *different*
	// unique violation, with the escalation checks never reaching the runner.
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("bob creating alice's organisation = %v, want ErrAlreadyExists", err)
	}

	// Bob is not a member.
	if _, err := db.RoleIn(ctx, bob, "taken-org"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob has a role in alice's organisation: %v", err)
	}
	// And Alice's display name was not rewritten.
	got, err := db.OrgForUser(ctx, alice, "taken-org")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.DisplayName != "Alice's Company" {
		t.Errorf("display name = %q, want Alice's — a failed create rewrote it", got.DisplayName)
	}
}

// Two creates for one account take turns.
//
// # Why this holds the lock itself rather than racing two goroutines
//
// The obvious test — start two creates at once against a limit of one, assert
// one fails — was written first and **passed with the lock removed**, three
// times running. The window between the count and the insert is well under a
// millisecond, so two goroutines simply never interleaved, and the test proved
// nothing while reading as though it proved the thing it was named for.
//
// This holds the account row exactly as CreateOrgForOwner does, then asserts a
// create blocks until it is released. That is deterministic, and it fails
// immediately if the lock is dropped: without it the create sails past a held
// row, which is the whole defect.
//
// A count is not a constraint, and under READ COMMITTED — which this pool uses
// — counting inside a transaction bounds nothing on its own. There is no
// constraint expressing "at most N rows per user" to fall back on, so the lock
// is the mechanism rather than a belt on braces.
func TestTwoCreatesForOneAccountTakeTurns(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	u := ownerFor(t, db, "lockprobe@example.com")

	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()

	var limit int
	if err := holder.QueryRow(ctx,
		`SELECT org_limit FROM cloud.users WHERE id = $1 FOR NO KEY UPDATE`, u).
		Scan(&limit); err != nil {
		t.Fatalf("hold the account row: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- db.CreateOrgForOwner(ctx, "lock-org", "", u, identity.RoleAdmin)
	}()

	select {
	case err := <-done:
		t.Fatalf("a create finished while another transaction held the account row "+
			"(%v) — the count is running outside the lock, so two creates can both "+
			"pass the limit", err)
	case <-time.After(500 * time.Millisecond):
		// Blocked, which is what makes the limit hold.
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the create failed once the row was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the create never completed after the row was released")
	}
}

// Concurrent creates for *different* accounts do not block each other.
//
// The lock is per account by design. One that serialised every create in the
// deployment would be a correctness fix that quietly became a throughput
// ceiling, and nothing would report it.
func TestCreatesForDifferentAccountsDoNotBlock(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	a := ownerFor(t, db, "parallel-a@example.com")
	b := ownerFor(t, db, "parallel-b@example.com")

	holder, err := db.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()

	var limit int
	if err := holder.QueryRow(ctx,
		`SELECT org_limit FROM cloud.users WHERE id = $1 FOR NO KEY UPDATE`, a).
		Scan(&limit); err != nil {
		t.Fatalf("hold a's row: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- db.CreateOrgForOwner(ctx, "b-org", "", b, identity.RoleAdmin) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("b's create failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("b's create blocked on a lock held for a different account")
	}
}

// The limit counts what this account created, not what it can act in.
//
// If it counted admin memberships, somebody adding you to their organisation
// would consume your quota — and would do it through AddMember, which never
// touches the create path's lock.
func TestTheLimitCountsCreationNotMembership(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	owner := ownerFor(t, db, "counts-owner@example.com")
	guest := ownerFor(t, db, "counts-guest@example.com")
	if _, err := db.pool.Exec(ctx, `UPDATE cloud.users SET org_limit = 1 WHERE id = $1`, guest); err != nil {
		t.Fatalf("set limit: %v", err)
	}

	if err := db.CreateOrgForOwner(ctx, "counts-org", "", owner, identity.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	// The guest is made an admin of somebody else's organisation.
	if err := db.AddMember(ctx, guest, "counts-org", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}

	// Their own single slot must be untouched.
	if err := db.CreateOrgForOwner(ctx, "guests-own-org", "", guest, identity.RoleAdmin); err != nil {
		t.Errorf("the guest could not create their own organisation: %v — somebody "+
			"else's membership consumed their quota", err)
	}
}

// At the limit, the refusal says so rather than blaming the name.
func TestTheLimitIsReportedAsItself(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u := ownerFor(t, db, "atlimit@example.com")
	if _, err := db.pool.Exec(ctx, `UPDATE cloud.users SET org_limit = 0 WHERE id = $1`, u); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	err := db.CreateOrgForOwner(ctx, "would-be-org", "", u, identity.RoleAdmin)
	if !errors.Is(err, ErrOrgLimitReached) {
		t.Fatalf("create at the limit = %v, want ErrOrgLimitReached", err)
	}
}

// An invalid or reserved name never reaches the database.
func TestBadNamesAreRefusedBeforeTheDatabase(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	u := ownerFor(t, db, "badnames@example.com")

	for _, name := range []string{"", "Bad-Name", "-leading", "under_score", "admin", strings.Repeat("a", 64)} {
		err := db.CreateOrgForOwner(ctx, name, "", u, identity.RoleAdmin)
		if err == nil {
			t.Errorf("CreateOrgForOwner(%q) succeeded", name)
			continue
		}
		if strings.Contains(err.Error(), "constraint") {
			t.Errorf("CreateOrgForOwner(%q) surfaced a constraint error: %v", name, err)
		}
	}
}

// A created organisation is queued, owned, and visible to its creator.
func TestACreatedOrganisationIsQueuedAndVisible(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	u := ownerFor(t, db, "visible@example.com")

	if err := db.CreateOrgForOwner(ctx, "visible-org", "Visible", u, identity.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}

	p, err := db.ProvisioningFor(ctx, "visible-org")
	if err != nil {
		t.Fatalf("the organisation was not queued: %v", err)
	}
	if p.State != StatePending {
		t.Errorf("state = %q, want pending", p.State)
	}

	orgs, err := db.OrgsForUser(ctx, u)
	if err != nil {
		t.Fatalf("OrgsForUser: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("OrgsForUser returned %d, want 1", len(orgs))
	}
	got := orgs[0]
	if got.Name != "visible-org" || got.DisplayName != "Visible" || got.Role != identity.RoleAdmin {
		t.Errorf("summary = %+v", got)
	}
	if got.State != StatePending {
		t.Errorf("state = %q, want pending", got.State)
	}
	if !got.CreatedByMe {
		t.Error("CreatedByMe is false for the account that created it")
	}
	if got.ConsoleURL != "" {
		t.Errorf("console URL = %q before provisioning", got.ConsoleURL)
	}
}

// An organisation with no queue row still appears.
//
// `acme` and `walkthru2` were registered by hand and have no row in
// org_provisioning. An INNER JOIN would drop them out of their members' lists
// entirely — they are provisioned, just not by the provisioner.
func TestAnOrganisationWithNoQueueRowStillAppears(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	u := ownerFor(t, db, "handmade@example.com")

	if err := db.CreateOrg(ctx, "handmade-org", "By Hand"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.AddMember(ctx, u, "handmade-org", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}

	orgs, err := db.OrgsForUser(ctx, u)
	if err != nil {
		t.Fatalf("OrgsForUser: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("an organisation with no queue row was dropped: got %d", len(orgs))
	}
	if orgs[0].State != "" {
		t.Errorf("state = %q, want empty for an organisation nothing queued", orgs[0].State)
	}
	if orgs[0].CreatedByMe {
		t.Error("CreatedByMe is true for an organisation created before the column existed")
	}
}

// Reading an organisation you are not in is indistinguishable from one that
// does not exist.
func TestANonMemberCannotTellAnOrganisationExists(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	owner := ownerFor(t, db, "private-owner@example.com")
	outsider := ownerFor(t, db, "outsider@example.com")
	if err := db.CreateOrgForOwner(ctx, "private-org", "", owner, identity.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, realErr := db.OrgForUser(ctx, outsider, "private-org")
	_, fakeErr := db.OrgForUser(ctx, outsider, "no-such-org")
	if !errors.Is(realErr, ErrNotFound) || !errors.Is(fakeErr, ErrNotFound) {
		t.Fatalf("want ErrNotFound for both, got %v and %v", realErr, fakeErr)
	}
	if realErr.Error() == fakeErr.Error() {
		return // identical, which is the point
	}
	// Different text is only acceptable if it is the name echoed back.
	if !strings.Contains(realErr.Error(), "private-org") || !strings.Contains(fakeErr.Error(), "no-such-org") {
		t.Errorf("the two refusals differ in a way that reveals existence:\n %v\n %v", realErr, fakeErr)
	}
}
