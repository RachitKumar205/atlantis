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

	// The error alone does not prove the rollback; these two do.
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

// Claiming sets the state to 'provisioning'. A predicate that only looks at
// pending and failed stops seeing a row the moment it is claimed, so a
// provisioner that dies mid-work wedges its organisation for ever and the lease
// column is never evaluated.
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

// The mirror of the test above: without it, that one passes against a predicate
// that claims everything.
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

// The claim is one statement. A select followed by an update has a window
// between them in which a second provisioner takes the same row.
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

// Without a backoff a failed organisation is claimable on the next tick, so a
// permanent fault becomes a loop that provisions nothing.
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

// A retry delay of zero means claimable immediately, so it is refused.
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
	// Sub-millisecond rounds to zero in the query.
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

// A provisioner whose lease expired must not extend the claim somebody else
// now holds.
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

// Nothing else looks at a ready row: the claim predicate covers pending, failed
// and expired-provisioning. Without ReadyOrgs an organisation whose cluster was
// rebuilt stays ready for ever while serving nothing.
func TestReadyOrganisationsCanBeListedAndRequeued(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "reconcile-owner@example.com")

	for _, org := range []string{"recon-a", "recon-b", "recon-c"} {
		if err := db.CreateOrgWithOwner(ctx, org, "", owner, identity.RoleAdmin); err != nil {
			t.Fatalf("create %s: %v", org, err)
		}
	}

	// Only the two that reach ready should be listed. The third stays pending,
	// which is what proves the query filters rather than returning everything.
	for _, org := range []string{"recon-a", "recon-b"} {
		if _, err := db.ClaimForProvisioning(ctx, "t", time.Minute); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := db.MarkProvisioned(ctx, org); err != nil {
			t.Fatalf("mark %s: %v", org, err)
		}
	}

	ready, err := db.ReadyOrgs(ctx)
	if err != nil {
		t.Fatalf("ReadyOrgs: %v", err)
	}
	got := map[string]bool{}
	for _, o := range ready {
		got[o] = true
	}
	if !got["recon-a"] || !got["recon-b"] {
		t.Errorf("ReadyOrgs = %v, want it to include recon-a and recon-b", ready)
	}
	if got["recon-c"] {
		t.Errorf("ReadyOrgs = %v, but recon-c was never provisioned", ready)
	}

	if err := db.Requeue(ctx, "recon-a", "the cluster no longer has this organisation"); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	p, err := db.ProvisioningFor(ctx, "recon-a")
	if err != nil {
		t.Fatalf("ProvisioningFor: %v", err)
	}
	if p.State != StatePending {
		t.Errorf("state = %q after requeue, want pending", p.State)
	}
	if p.NextAttemptAfter != nil {
		t.Errorf("a requeued organisation carries a backoff (%v); it should be "+
			"claimable now, since nothing about it failed", p.NextAttemptAfter)
	}
	// attempts is kept, so a row rebuilt repeatedly still shows its count.
	if p.Attempts == 0 {
		t.Error("requeue reset the attempt count, hiding how often this organisation has been rebuilt")
	}

	// And it is claimable again.
	c, err := db.ClaimForProvisioning(ctx, "t2", time.Minute)
	if err != nil {
		t.Fatalf("a requeued organisation could not be claimed: %v", err)
	}
	if c.Org != "recon-a" {
		t.Errorf("claimed %q, want recon-a", c.Org)
	}
}

// Reconciliation reads a list, checks the cluster, then writes. In between, an
// operator may have reset the row by hand and a provisioner may have claimed
// it. Without the guard this would stamp pending over an active claim and two
// provisioners would build the same organisation.
func TestRequeueRefusesARowThatIsNoLongerReady(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	owner := ownerFor(t, db, "requeue-guard@example.com")

	if err := db.CreateOrgWithOwner(ctx, "guard-org", "", owner, identity.RoleAdmin); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Still pending, never provisioned.
	if err := db.Requeue(ctx, "guard-org", "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Requeue on a pending row = %v, want ErrNotFound so a concurrent "+
			"claim is not overwritten", err)
	}

	if _, err := db.ClaimForProvisioning(ctx, "t", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Now claimed and provisioning — still not ready.
	if err := db.Requeue(ctx, "guard-org", "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Requeue on a claimed row = %v, want ErrNotFound", err)
	}
	p, err := db.ProvisioningFor(ctx, "guard-org")
	if err != nil {
		t.Fatalf("ProvisioningFor: %v", err)
	}
	if p.State != StateProvisioning {
		t.Errorf("state = %q, want provisioning — requeue overwrote an active claim", p.State)
	}
}
