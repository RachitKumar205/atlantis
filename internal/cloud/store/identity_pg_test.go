package store

import (
	"context"
	"errors"
	"testing"
)

// Linking, unlinking, and creating an account from a provider.
//
// The properties here are the ones whose failure is silent: a link that reports
// success without moving, an unlink that reports the wrong thing after deleting
// two rows, and a half-created account left behind by a failed transaction.

const somePasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"

// Without `WHERE cloud.identities.user_id = EXCLUDED.user_id` the conflict
// branch updates provider_email and returns nil, so the caller is told the link
// was made while it still points at the other account.
func TestLinkingAClaimedProviderAccountIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	mine, err := db.CreateUser(ctx, "mine@example.com", "Mine", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs, err := db.CreateUser(ctx, "theirs@example.com", "Theirs", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := db.LinkIdentity(ctx, mine.ID, "github", "4242", "mine@example.com"); err != nil {
		t.Fatalf("first link: %v", err)
	}

	err = db.LinkIdentity(ctx, theirs.ID, "github", "4242", "theirs@example.com")
	if !errors.Is(err, ErrIdentityClaimed) {
		t.Fatalf("linking a claimed account returned %v, want ErrIdentityClaimed", err)
	}

	// And it did not move: the error alone does not prove the write was
	// refused.
	owner, err := db.UserByIdentity(ctx, "github", "4242")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if owner.ID != mine.ID {
		t.Fatalf("the link moved to %s; it belonged to %s", owner.ID, mine.ID)
	}
}

// Re-linking your own provider account is not an error, and refreshes the
// address shown for it.
func TestRelinkingYourOwnAccountSucceeds(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "again@example.com", "Again", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "7", "old@example.com"); err != nil {
		t.Fatalf("first link: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "7", "new@example.com"); err != nil {
		t.Fatalf("second link: %v", err)
	}

	linked, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("re-linking made %d rows, want 1", len(linked))
	}
	if linked[0].Email != "new@example.com" {
		t.Errorf("address is %q, want the one from the second link", linked[0].Email)
	}
}

func TestUnlinkingTheOnlyWayInIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "only@example.com", "Only", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "1", ""); err != nil {
		t.Fatalf("link: %v", err)
	}

	if err := db.UnlinkIdentity(ctx, u.ID, "github"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlink returned %v, want ErrLastSignInMethod", err)
	}
	linked, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 {
		t.Fatal("the refused unlink removed the row anyway, so the account is unreachable")
	}
}

// One user can hold two links from one provider, because the key is
// (provider, provider_subject). UnlinkIdentity deletes every row for the named
// provider, so a predicate counting all of the user's identities and asking for
// more than one is satisfied by those two, deletes both, and leaves an account
// with no password and no identity.
func TestTwoLinksFromOneProviderAreStillOneWayIn(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "twins@example.com", "Twins", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, subject := range []string{"1", "2"} {
		if err := db.LinkIdentity(ctx, u.ID, "github", subject, ""); err != nil {
			t.Fatalf("link %s: %v", subject, err)
		}
	}

	if err := db.UnlinkIdentity(ctx, u.ID, "github"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlink returned %v, want ErrLastSignInMethod", err)
	}
	linked, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 2 {
		t.Fatalf("%d links remain, want 2 — the account can no longer be reached", len(linked))
	}
}

// A second provider makes the first removable, and removing two rows must
// report success. A row count checked for exactly one falls through to the "was
// it even linked?" branch and answers ErrNotFound after deleting everything.
func TestUnlinkingIsAllowedWhenAnotherWayInRemains(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUser(ctx, "both@example.com", "Both", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Two GitHub links and one Google link, so the delete removes two rows.
	for _, subject := range []string{"1", "2"} {
		if err := db.LinkIdentity(ctx, u.ID, "github", subject, ""); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	if err := db.LinkIdentity(ctx, u.ID, "google", "g1", ""); err != nil {
		t.Fatalf("link google: %v", err)
	}

	if err := db.UnlinkIdentity(ctx, u.ID, "github"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	linked, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 || linked[0].Provider != "google" {
		t.Fatalf("after unlinking github, remaining links are %+v", linked)
	}

	// Now it is the only way in, and refused.
	if err := db.UnlinkIdentity(ctx, u.ID, "google"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlinking the last provider returned %v, want ErrLastSignInMethod", err)
	}
}

// A password is a way in, so a link may be removed while one exists.
func TestAPasswordMakesALinkRemovable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	hash := somePasswordHash
	u, err := db.CreateUser(ctx, "haspw@example.com", "Has", &hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "9", ""); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := db.UnlinkIdentity(ctx, u.ID, "github"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	linked, err := db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 0 {
		t.Fatal("the link survived an unlink that reported success")
	}
}

// User.HasPassword treats "" as no password. `password_hash IS NOT NULL` alone
// disagrees, and an account Go considers password-less could then unlink its
// only identity and be locked out. Nothing writes an empty hash today.
func TestAnEmptyPasswordHashIsNotAWayIn(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	empty := ""
	u, err := db.CreateUser(ctx, "blank@example.com", "Blank", &empty)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.HasPassword() {
		t.Fatal("an empty hash reports as a password in Go; this test assumes it does not")
	}
	if err := db.LinkIdentity(ctx, u.ID, "github", "11", ""); err != nil {
		t.Fatalf("link: %v", err)
	}

	if err := db.UnlinkIdentity(ctx, u.ID, "github"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlink returned %v, want ErrLastSignInMethod — the database and "+
			"HasPassword disagree about what an empty hash means", err)
	}
}

func TestUnlinkingWhatIsNotLinkedIsNotFound(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	hash := somePasswordHash
	u, err := db.CreateUser(ctx, "none@example.com", "None", &hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.UnlinkIdentity(ctx, u.ID, "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink returned %v, want ErrNotFound", err)
	}
}

func TestUnlinkingIsScopedToTheAccount(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	hash := somePasswordHash
	mine, err := db.CreateUser(ctx, "a@example.com", "A", &hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs, err := db.CreateUser(ctx, "b@example.com", "B", &hash)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, theirs.ID, "github", "77", ""); err != nil {
		t.Fatalf("link: %v", err)
	}

	if err := db.UnlinkIdentity(ctx, mine.ID, "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink returned %v, want ErrNotFound", err)
	}
	linked, err := db.IdentitiesOf(ctx, theirs.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 {
		t.Fatal("one account unlinked another account's provider")
	}
}

// An account made from a provider arrives verified and linked, together.
func TestCreatingAnAccountFromAProviderIsOneStep(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	u, err := db.CreateUserWithIdentity(ctx,
		"New@Example.com", "New Person", "google", "sub-1", "new@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "new@example.com" {
		t.Errorf("email stored as %q, not folded", u.Email)
	}
	// Verified at creation. Without this the account exists and cannot sign in,
	// because handleLogin refuses an unverified address.
	if u.EmailVerifiedAt == nil {
		t.Error("an account created from a provider is not marked verified")
	}
	if u.HasPassword() {
		t.Error("an account created from a provider has a password")
	}

	found, err := db.UserByIdentity(ctx, "google", "sub-1")
	if err != nil {
		t.Fatalf("resolve by identity: %v", err)
	}
	if found.ID != u.ID {
		t.Errorf("identity resolves to %s, want %s", found.ID, u.ID)
	}
}

// The user row is inserted first, so a failure at the identity insert has
// already written one. Without the rollback the address is occupied by an
// account nobody can reach, and the retry collides on the UNIQUE constraint.
func TestAFailedProviderSignUpLeavesNoAccount(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	first, err := db.CreateUser(ctx, "holder@example.com", "Holder", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.LinkIdentity(ctx, first.ID, "github", "contested", ""); err != nil {
		t.Fatalf("link: %v", err)
	}

	_, err = db.CreateUserWithIdentity(ctx,
		"newcomer@example.com", "Newcomer", "github", "contested", "")
	if !errors.Is(err, ErrIdentityClaimed) {
		t.Fatalf("create returned %v, want ErrIdentityClaimed", err)
	}

	if _, err := db.UserByEmail(ctx, "newcomer@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an account was left behind: UserByEmail returned %v, want ErrNotFound", err)
	}
	owner, err := db.UserByIdentity(ctx, "github", "contested")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if owner.ID != first.ID {
		t.Error("the contested identity changed owner")
	}
}

// An address that already has an account is refused, and no link is made.
func TestAProviderCannotTakeAnOccupiedAddress(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	hash := somePasswordHash
	if _, err := db.CreateUser(ctx, "taken@example.com", "Taken", &hash); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := db.CreateUserWithIdentity(ctx,
		"taken@example.com", "Impostor", "github", "impostor", "")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("create returned %v, want ErrAlreadyExists", err)
	}
	if _, err := db.UserByIdentity(ctx, "github", "impostor"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a link was created for an account that was never made")
	}
}
