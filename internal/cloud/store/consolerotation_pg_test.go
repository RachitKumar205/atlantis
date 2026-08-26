package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The console credential rotation request, which Cloud records and the
// provisioner acts on.
//
// The clear is where this goes wrong. It runs after work that takes time, so a
// request made inside that window can be discarded along with the one the pass
// handled, leaving the credential the operator asked twice to replace in use.
// The command reports success both times.

func rotatable(t *testing.T, db *Store, org string) {
	t.Helper()
	ctx := context.Background()
	u, err := db.CreateUser(ctx, org+"@example.test", "Owner", nil)
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

func TestRequestingARotationIsVisibleToTheProvisioner(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rotatable(t, db, "rotate-one")

	// Nothing outstanding to begin with, or the assertion below proves nothing.
	before, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := before["rotate-one"]; ok {
		t.Fatal("a freshly provisioned organisation already has a rotation request")
	}

	if err := db.RequestConsoleRotation(ctx, "rotate-one"); err != nil {
		t.Fatalf("request: %v", err)
	}

	after, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := after["rotate-one"]
	if !ok {
		t.Fatal("the request is not visible to the provisioner, so nothing would act on it")
	}
	if at.IsZero() {
		t.Error("the request has no timestamp; the clear guard has nothing to compare against")
	}
}

func TestRequestingARotationForAnUnknownOrganisationFails(t *testing.T) {
	db := newTestStore(t)
	err := db.RequestConsoleRotation(context.Background(), "rotate-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestClearingARequestRemovesIt(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rotatable(t, db, "rotate-clear")

	if err := db.RequestConsoleRotation(ctx, "rotate-clear"); err != nil {
		t.Fatal(err)
	}
	reqs, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := reqs["rotate-clear"]

	if err := db.ClearConsoleRotationRequest(ctx, "rotate-clear", seen); err != nil {
		t.Fatalf("clear: %v", err)
	}
	after, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["rotate-clear"]; ok {
		t.Error("the request survived being cleared, so the provisioner rotates again every pass")
	}
}

// The provisioner reads the request, spends time reissuing certificates and
// re-registering them, then clears. An unconditional SET NULL would discard a
// request made inside that window, leaving the credential the operator asked
// twice to replace still in service and nothing to show a request was dropped.
func TestClearingDoesNotDiscardARequestMadeDuringTheRotation(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rotatable(t, db, "rotate-race")

	if err := db.RequestConsoleRotation(ctx, "rotate-race"); err != nil {
		t.Fatal(err)
	}
	reqs, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := reqs["rotate-race"]

	// The operator asks again while the rotation is under way, moving the
	// timestamp past what the pass observed.
	//
	// A sleep rather than a fabricated timestamp: the guard compares against
	// what the database wrote. Postgres timestamps have microsecond resolution,
	// so 5ms separates the two.
	time.Sleep(5 * time.Millisecond)
	if err := db.RequestConsoleRotation(ctx, "rotate-race"); err != nil {
		t.Fatal(err)
	}

	// The provisioner finishes the first rotation and clears what it saw.
	if err := db.ClearConsoleRotationRequest(ctx, "rotate-race", seen); err != nil {
		t.Fatalf("clear: %v", err)
	}

	after, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at, ok := after["rotate-race"]
	if !ok {
		t.Fatal("the second request was discarded along with the first; the operator " +
			"asked twice and the credential was replaced once")
	}
	if !at.After(seen) {
		t.Errorf("the outstanding request is stamped %s, which is not after the one "+
			"that was handled (%s)", at, seen)
	}
}

// The provisioner forces a rotation for everything this returns, so a query
// that returned every row would reissue every organisation's credentials on
// every reconcile pass.
func TestOnlyOrganisationsWithARequestAreReported(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()
	rotatable(t, db, "rotate-asked")
	rotatable(t, db, "rotate-quiet")

	if err := db.RequestConsoleRotation(ctx, "rotate-asked"); err != nil {
		t.Fatal(err)
	}

	reqs, err := db.ConsoleRotationRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reqs["rotate-quiet"]; ok {
		t.Error("an organisation nobody asked about is reported as having a request; " +
			"the provisioner would force a rotation for the entire fleet every pass")
	}
	if _, ok := reqs["rotate-asked"]; !ok {
		t.Error("the organisation that asked is missing")
	}
}
