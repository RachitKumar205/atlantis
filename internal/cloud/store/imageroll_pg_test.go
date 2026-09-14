package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The image roll request, which Cloud records and the provisioner acts on.
//
// Two things make this different from the console rotation beside it. The
// request carries which images it covers, because rolling Postgres restarts a
// single-instance database and rolling the server does not. And the roll takes
// minutes rather than seconds, which widens the window in which a second
// request can arrive and be discarded with the first.

func rollable(t *testing.T, db *Store, org string) {
	t.Helper()
	ctx := context.Background()
	u, err := db.CreateUser(ctx, org+"@example.test", "Owner", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrgWithOwner(ctx, org, org, u.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkProvisioned(ctx, org); err != nil {
		t.Fatal(err)
	}
}

func TestARollRequestCarriesItsKindsToTheProvisioner(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-one")

	// Nothing outstanding to begin with, or the assertion below proves nothing.
	before, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before["roll-one"]; ok {
		t.Fatal("a freshly provisioned organisation already has a roll request")
	}

	if err := db.RequestImageRoll(ctx, "roll-one", []string{"server", "signer"}); err != nil {
		t.Fatalf("request: %v", err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := after["roll-one"]
	if !ok {
		t.Fatal("the request is not visible to the provisioner, so nothing would act on it")
	}
	if len(got.Kinds) != 2 || got.Kinds[0] != "server" || got.Kinds[1] != "signer" {
		t.Errorf("kinds = %v, want [server signer] — a provisioner reading this would "+
			"roll something other than what was asked for", got.Kinds)
	}
	if got.RequestedAt.IsZero() {
		t.Error("no timestamp, so the clear has nothing to compare against and a " +
			"request made during the roll would be discarded")
	}
}

// Asking again replaces the kinds.
//
// The operator's second command is the current instruction. Adding to the first
// would mean `cloud org roll -org x` after `-org x -postgres -yes` still rolled
// Postgres, which is the restart they stopped asking for.
func TestASecondRequestReplacesTheKinds(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-twice")

	if err := db.RequestImageRoll(ctx, "roll-twice", []string{"server", "signer", "postgres"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestImageRoll(ctx, "roll-twice", []string{"server"}); err != nil {
		t.Fatal(err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := after["roll-twice"].Kinds
	if len(got) != 1 || got[0] != "server" {
		t.Errorf("kinds = %v, want [server]; the withdrawn postgres roll would still "+
			"restart the database", got)
	}
}

// A request made while a roll was running survives the clear.
//
// A roll takes minutes. Clearing unconditionally would discard the second
// request along with the first, and the command that made it reported success.
func TestClearingKeepsARequestMadeDuringTheRoll(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-race")

	if err := db.RequestImageRoll(ctx, "roll-race", []string{"server"}); err != nil {
		t.Fatal(err)
	}
	first, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := first["roll-race"].RequestedAt

	// The operator asks again, for more, while the pass is still working.
	time.Sleep(10 * time.Millisecond)
	if err := db.RequestImageRoll(ctx, "roll-race", []string{"server", "postgres"}); err != nil {
		t.Fatal(err)
	}

	// The pass finishes and clears what it saw.
	if err := db.ClearImageRollRequest(ctx, "roll-race", seen); err != nil {
		t.Fatalf("clear: %v", err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := after["roll-race"]
	if !ok {
		t.Fatal("the second request was cleared with the first; the operator asked " +
			"for a Postgres roll, was told it was queued, and nothing will do it")
	}
	if len(got.Kinds) != 2 {
		t.Errorf("kinds = %v, want the second request's two", got.Kinds)
	}
}

// Clearing the request that was seen removes it.
func TestClearingTheObservedRequestRemovesIt(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-clear")

	if err := db.RequestImageRoll(ctx, "roll-clear", []string{"server"}); err != nil {
		t.Fatal(err)
	}
	reqs, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClearImageRollRequest(ctx, "roll-clear", reqs["roll-clear"].RequestedAt); err != nil {
		t.Fatal(err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["roll-clear"]; ok {
		t.Error("the request survived its own clear, so the organisation rolls again every pass")
	}
}

// An organisation with no provisioning row is refused rather than recorded.
//
// `cloud org roll -org typo` would otherwise report a queued roll for a name
// nothing will ever match.
func TestRollingAnUnknownOrganisationIsRefused(t *testing.T) {
	db := newTestStore(t)
	err := db.RequestImageRoll(context.Background(), "no-such-org", []string{"server"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// A failed attempt is counted, and a fresh request resets the count.
//
// The provisioner orders on the count before the age, so this is what stops one
// request that cannot succeed holding the only slot on every pass.
func TestAFailedAttemptIsCountedAndANewRequestResetsIt(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-attempts")

	if err := db.RequestImageRoll(ctx, "roll-attempts", []string{"server"}); err != nil {
		t.Fatal(err)
	}
	reqs, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := reqs["roll-attempts"].Attempts; got != 0 {
		t.Fatalf("a fresh request starts at %d attempts, want 0", got)
	}
	seen := reqs["roll-attempts"].RequestedAt

	for i := 0; i < 2; i++ {
		if err := db.RecordImageRollFailure(ctx, "roll-attempts", seen); err != nil {
			t.Fatal(err)
		}
	}
	reqs, err = db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := reqs["roll-attempts"].Attempts; got != 2 {
		t.Errorf("attempts = %d after two failures, want 2; the request would keep "+
			"its place at the head of the queue", got)
	}

	// Asking again is a new instruction, and must not inherit the failures of
	// the request it replaced — that would send a first attempt to the back.
	if err := db.RequestImageRoll(ctx, "roll-attempts", []string{"server"}); err != nil {
		t.Fatal(err)
	}
	reqs, err = db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := reqs["roll-attempts"].Attempts; got != 0 {
		t.Errorf("attempts = %d after a new request, want 0", got)
	}
}

// A failure counted against a superseded request does not reach the new one.
func TestAFailureDoesNotLandOnARequestMadeAfterIt(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-late")

	if err := db.RequestImageRoll(ctx, "roll-late", []string{"server"}); err != nil {
		t.Fatal(err)
	}
	first, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := first["roll-late"].RequestedAt

	time.Sleep(10 * time.Millisecond)
	if err := db.RequestImageRoll(ctx, "roll-late", []string{"server", "postgres"}); err != nil {
		t.Fatal(err)
	}
	// The pass that was working on the first request now fails.
	if err := db.RecordImageRollFailure(ctx, "roll-late", seen); err != nil {
		t.Fatal(err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := after["roll-late"].Attempts; got != 0 {
		t.Errorf("attempts = %d on a request made after the failure, want 0", got)
	}
}

// Cancelling withdraws a queued roll and reports whether there was one.
//
// The per-pass cap exists so a fleet-wide roll can be stopped after watching the
// first organisation. Without this the only stop is scaling the provisioner down.
func TestCancellingWithdrawsAQueuedRoll(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-cancel")

	// Nothing queued: reports false rather than claiming a withdrawal.
	was, err := db.CancelImageRoll(ctx, "roll-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if was {
		t.Error("cancelling an organisation with no queued roll reported a withdrawal")
	}

	if err := db.RequestImageRoll(ctx, "roll-cancel", []string{"server", "postgres"}); err != nil {
		t.Fatal(err)
	}
	was, err = db.CancelImageRoll(ctx, "roll-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if !was {
		t.Error("cancelling a queued roll reported nothing was queued")
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["roll-cancel"]; ok {
		t.Error("the roll is still queued after being cancelled, so the database " +
			"restart the operator stopped still happens")
	}
}

// Deleting an organisation withdraws any queued roll.
//
// The provisioner visits ready organisations only, so a request kept through a
// deletion does nothing until a restore — and then restarts the database on an
// intent weeks old.
func TestDeletingAnOrganisationWithdrawsItsRoll(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-deleted")

	if err := db.RequestImageRoll(ctx, "roll-deleted", []string{"postgres"}); err != nil {
		t.Fatal(err)
	}
	owner, err := db.UserByEmail(ctx, "roll-deleted@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SoftDeleteOrg(ctx, "roll-deleted", owner.ID, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	after, err := db.ImageRollRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["roll-deleted"]; ok {
		t.Error("a deleted organisation kept its queued roll; restoring it would " +
			"restart the database on a request nobody remembers making")
	}
}

// The constraint holding the two request columns together is real.
//
// Without it a row can carry a timestamp naming no kinds, which the provisioner
// clears without doing anything — a request that reports queued and never runs.
func TestTheRollColumnsCannotBeSetApart(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-check")

	_, err := db.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_requested_at = NOW()
		 WHERE org = $1
	`, "roll-check")
	if err == nil {
		t.Error("a timestamp with no kinds was accepted; org_provisioning_image_roll_complete " +
			"is not enforcing what its comment claims")
	}

	_, err = db.pool.Exec(ctx, `
		UPDATE cloud.org_provisioning
		   SET image_roll_kinds = ARRAY['server']
		 WHERE org = $1
	`, "roll-check")
	if err == nil {
		t.Error("kinds with no timestamp were accepted, so the provisioner would never see them")
	}
}

// A request with no kinds is refused.
//
// The column is NOT NULL whenever the timestamp is set, and a row naming no
// kind is one the provisioner clears without doing anything — so refusing here
// is what makes the command's report true.
func TestARollWithNoKindsIsRefused(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rollable(t, db, "roll-empty")

	if err := db.RequestImageRoll(ctx, "roll-empty", nil); err == nil {
		t.Error("a request naming no image kind was accepted")
	}
}
