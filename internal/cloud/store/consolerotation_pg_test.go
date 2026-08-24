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
// # What is actually at risk here
//
// Not much, on the happy path: a column is set and later cleared. The one place
// this can go quietly wrong is the clear, because it runs after work that takes
// time, and an operator during an incident is exactly the person likely to ask
// twice. Losing the second request means the credential they asked twice to
// replace is the one still in use, and nothing reports that — the command
// printed success both times.

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

// An organisation that does not exist is reported rather than silently accepted.
func TestRequestingARotationForAnUnknownOrganisationFails(t *testing.T) {
	db := newTestStore(t)
	err := db.RequestConsoleRotation(context.Background(), "rotate-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// Clearing removes the request the provisioner handled.
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

// The one that matters: a request made while the rotation was running survives.
//
// The provisioner reads the request, spends time reissuing certificates and
// re-registering them, then clears. An operator who asks again inside that
// window is asking for a rotation that starts after their second request — and
// an unconditional `SET NULL` would discard it, leaving the credential they
// asked twice to replace still in service with nothing to show a request was
// dropped.
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

	// The operator asks again while the rotation is under way. NOW() moves the
	// timestamp past what the pass observed, which is the whole mechanism.
	//
	// A sleep rather than a fabricated timestamp: the guard compares against
	// what the database wrote, so a test that wrote its own would be testing its
	// own arithmetic. Postgres timestamps have microsecond resolution, so this
	// only has to be longer than that.
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

// Only organisations with a request are reported.
//
// The provisioner forces a rotation for everything this returns, so a query
// that returned every row would rotate the whole fleet on every reconcile pass
// — which works, and quietly reissues every organisation's credentials every
// five minutes.
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
