package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The provisioning queue, against a real database.
//
// The fixture does not truncate between tests, so every organisation and email
// below is unique to its test. See newTestStore.

func ownerFor(t *testing.T, db *Store, email string) string {
	t.Helper()
	u, err := db.CreateUser(context.Background(), email, "Owner", nil)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	return u.ID
}

// All three rows or none. Each pair without the third is a state the product
// cannot recover from: no queue row means nobody ever provisions it, no
// membership means nobody can enter it.
func TestCreatingAnOrganisationQueuesItAndGrantsItsOwner(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "queue-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "queued", "Queued", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	role, err := db.RoleIn(ctx, owner, "queued")
	if err != nil {
		t.Fatalf("the owner has no membership: %v", err)
	}
	if role != identity.RoleAdmin {
		t.Errorf("owner role = %q, want admin", role)
	}

	p, err := db.ProvisioningFor(ctx, "queued")
	if err != nil {
		t.Fatalf("no queue row: %v", err)
	}
	if p.State != StatePending {
		t.Errorf("state = %q, want pending", p.State)
	}
	if p.Attempts != 0 {
		t.Errorf("attempts = %d on a fresh row, want 0", p.Attempts)
	}
}

// An owner who does not exist must take the whole write down. Otherwise the
// organisation exists, unqueued and unowned, and nothing reports it.
func TestAnOrganisationIsNotCreatedWithoutItsOwner(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	err := db.CreateOrgWithOwner(ctx, "ownerless", "", "usr_does_not_exist", identity.RoleAdmin)
	if err == nil {
		t.Fatal("an organisation was created for an owner that does not exist")
	}

	// The rollback is the point: not merely that it errored, but that it left
	// nothing behind.
	if _, err := db.ConsoleURL(ctx, "ownerless"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the organisation row survived a failed create: %v", err)
	}
	if _, err := db.ProvisioningFor(ctx, "ownerless"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the queue row survived a failed create: %v", err)
	}
}

// `cloud org create` is documented as safe to re-run. Re-running it must not
// send a working organisation back through provisioning.
func TestCreatingAnOrganisationTwiceDoesNotRequeueAReadyOne(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "rerun-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "rerun", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioned(ctx, "rerun"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrgWithOwner(ctx, "rerun", "Renamed", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	p, err := db.ProvisioningFor(ctx, "rerun")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateReady {
		t.Fatalf("state = %q after re-creating a ready organisation, want ready — "+
			"re-running the command would re-provision it", p.State)
	}
}

// The property a whole class of queue bugs hides behind.
//
// Claiming sets the state to `provisioning`. A predicate that only looks at
// pending and failed therefore stops seeing a row the moment it is claimed, so
// a provisioner that dies mid-work wedges its organisation for ever — and the
// lease column that was supposed to prevent that is never even evaluated.
func TestAnExpiredLeaseIsClaimableAgain(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "lease-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "leased", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	// A lease that has already expired, which is what a crashed provisioner
	// leaves behind a minute later.
	first, err := db.ClaimForProvisioning(ctx, "provisioner-a", -1*time.Minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Org != "leased" {
		t.Fatalf("claimed %q, want leased", first.Org)
	}

	second, err := db.ClaimForProvisioning(ctx, "provisioner-b", time.Minute)
	if err != nil {
		t.Fatalf("an expired lease was not reclaimable, so a crashed provisioner "+
			"wedges its organisation for ever: %v", err)
	}
	if second.Org != "leased" {
		t.Fatalf("reclaimed %q, want leased", second.Org)
	}
	if second.Attempts <= first.Attempts {
		t.Errorf("attempts did not advance on reclaim: %d then %d — a crash loop "+
			"would never back off", first.Attempts, second.Attempts)
	}
}

// A live lease is not claimable. The mirror of the test above, and without it
// that one passes against a predicate that claims everything.
func TestALiveLeaseIsNotClaimable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "live-lease@example.com")

	if err := db.CreateOrgWithOwner(ctx, "livelease", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForProvisioning(ctx, "provisioner-a", 5*time.Minute); err != nil {
		t.Fatal(err)
	}

	_, err := db.ClaimForProvisioning(ctx, "provisioner-b", time.Minute)
	if !errors.Is(err, ErrNothingToProvision) {
		t.Fatalf("a second provisioner took a live lease: %v", err)
	}
}

// Two claimants, one organisation. This is what the single-statement claim
// buys: a select followed by an update has a window between them.
func TestTwoProvisionersNeverClaimTheSameOrganisation(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "contend-owner@example.com")

	for _, org := range []string{"contend-a", "contend-b"} {
		if err := db.CreateOrgWithOwner(ctx, org, "", owner, identity.RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		c, err := db.ClaimForProvisioning(ctx, "provisioner", 5*time.Minute)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if seen[c.Org] {
			t.Fatalf("%s was claimed twice while still leased", c.Org)
		}
		seen[c.Org] = true
	}

	// Both taken, so the queue is empty rather than serving one of them again.
	if _, err := db.ClaimForProvisioning(ctx, "provisioner", time.Minute); !errors.Is(err, ErrNothingToProvision) {
		t.Errorf("a third claim returned something: %v", err)
	}
}

// Without a backoff a failed organisation is claimable on the very next tick,
// so a permanent fault becomes a loop that provisions nothing. atlantis.jobs
// has exactly that shape; this must not.
func TestAFailedOrganisationWaitsForItsBackoff(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "backoff-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "backoff", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForProvisioning(ctx, "provisioner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioningFailed(ctx, "backoff", "the image does not exist", time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ClaimForProvisioning(ctx, "provisioner", time.Minute); !errors.Is(err, ErrNothingToProvision) {
		t.Fatalf("a failed organisation was claimable before its backoff elapsed: %v", err)
	}

	p, err := db.ProvisioningFor(ctx, "backoff")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateFailed {
		t.Errorf("state = %q, want failed", p.State)
	}
	if p.LastError == "" {
		t.Error("the failure recorded no reason, so nothing on a screen can say why")
	}

	// And once the wait has passed it must come back — failed is not terminal.
	if err := db.MarkProvisioningFailed(ctx, "backoff", "still broken", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := db.ClaimForProvisioning(ctx, "provisioner", time.Minute); err != nil {
		t.Fatalf("a failed organisation never became claimable again: %v", err)
	}
}

// A retry delay of zero would reintroduce the busy loop, so it is refused
// rather than quietly treated as "now".
func TestFailingWithoutABackoffIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "nobackoff@example.com")

	if err := db.CreateOrgWithOwner(ctx, "nobackoff", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioningFailed(ctx, "nobackoff", "boom", 0); err == nil {
		t.Error("a failure with no retry delay was accepted")
	}
	// Sub-millisecond rounds to zero in the query, so it is the same failure
	// wearing a positive number.
	if err := db.MarkProvisioningFailed(ctx, "nobackoff", "boom", time.Microsecond); err == nil {
		t.Error("a sub-millisecond retry delay was accepted, which means claimable immediately")
	}
}

// An empty queue is the ordinary case, and a worker loop has to tell it apart
// from a failure.
func TestAnEmptyQueueIsItsOwnAnswer(t *testing.T) {
	db := newTestStore(t)
	_, err := db.ClaimForProvisioning(context.Background(), "provisioner", time.Minute)
	if !errors.Is(err, ErrNothingToProvision) {
		t.Fatalf("an empty queue reported %v, want ErrNothingToProvision", err)
	}
}

// Extending a lease somebody else now holds is exactly when a stalled
// provisioner would most want to, and exactly when it must not.
func TestExtendingSomebodyElsesLeaseIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "extend-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "extend", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForProvisioning(ctx, "provisioner-a", time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := db.ExtendLease(ctx, "extend", "provisioner-a", 5*time.Minute); err != nil {
		t.Fatalf("the holder could not extend its own lease: %v", err)
	}
	if err := db.ExtendLease(ctx, "extend", "provisioner-b", 5*time.Minute); err == nil {
		t.Error("a provisioner extended a lease it does not hold")
	}
}

// Marking a ready organisation must clear the lease, or the row keeps looking
// claimed to anybody reading the table during an incident.
func TestASuccessfulProvisionReleasesItsLease(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "release-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "release", "", owner, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimForProvisioning(ctx, "provisioner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioned(ctx, "release"); err != nil {
		t.Fatal(err)
	}

	p, err := db.ProvisioningFor(ctx, "release")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateReady {
		t.Errorf("state = %q, want ready", p.State)
	}
	if p.ClaimedUntil != nil {
		t.Error("a ready organisation still holds a lease")
	}
	if p.ClaimedBy != "" {
		t.Errorf("a ready organisation still names a claimant: %q", p.ClaimedBy)
	}
}

func TestTheQueueRefusesAnUnusableRequest(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "refuse-owner@example.com")

	if err := db.CreateOrgWithOwner(ctx, "", "", owner, identity.RoleAdmin); err == nil {
		t.Error("an organisation with no name was created")
	}
	if err := db.CreateOrgWithOwner(ctx, "noowner", "", "", identity.RoleAdmin); err == nil {
		t.Error("an organisation with no owner was created")
	}
	if err := db.CreateOrgWithOwner(ctx, "badrole", "", owner, identity.Role("root")); err == nil {
		t.Error("an organisation was created with a role the product does not understand")
	}
	if _, err := db.ClaimForProvisioning(ctx, "", time.Minute); err == nil {
		t.Error("an unnamed claimant took a lease")
	}
}
