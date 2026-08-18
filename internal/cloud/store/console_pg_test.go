package store

import (
	"context"
	"errors"
	"testing"
)

// Where an organisation's console lives, and what happens when nobody has said.

// An organisation starts with no console and says so.
//
// The distinction matters at /authorize: "no such organisation" and "nothing
// registered for it" send an operator to different places, and one error for
// both would send them to the wrong one.
func TestAnOrganisationStartsWithNoConsole(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateOrg(ctx, "fresh", "Fresh"); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := db.ConsoleURL(ctx, "fresh")
	if !errors.Is(err, ErrNoConsole) {
		t.Fatalf("ConsoleURL returned %v, want ErrNoConsole", err)
	}

	// And an organisation that does not exist is a different answer.
	if _, err := db.ConsoleURL(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsoleURL for a missing org returned %v, want ErrNotFound", err)
	}
}

// Registering a console makes it readable, and re-registering replaces it.
func TestRegisteringAConsoleIsReadableAndReplaceable(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.SetConsoleURL(ctx, "acme", "https://acme.example"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := db.ConsoleURL(ctx, "acme")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "https://acme.example" {
		t.Fatalf("console is %q", got)
	}

	// Moving a console is a re-run of the same command, not a second row.
	if err := db.SetConsoleURL(ctx, "acme", "https://acme2.example"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	got, err = db.ConsoleURL(ctx, "acme")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "https://acme2.example" {
		t.Fatalf("console is %q after re-registering", got)
	}
}

// A trailing slash is trimmed before it is stored.
//
// The value is compared against a console's CLOUD_AUDIENCE for exact equality
// and concatenated with a path to build a redirect, so a stray slash is every
// sign-in for that organisation failing over a character nobody can see.
func TestAConsoleURLIsStoredWithoutATrailingSlash(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateOrg(ctx, "slashed", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.SetConsoleURL(ctx, "slashed", "  https://acme.example/  "); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := db.ConsoleURL(ctx, "slashed")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "https://acme.example" {
		t.Fatalf("console is %q, want it trimmed", got)
	}
}

// Setting a console for an organisation that does not exist is refused.
//
// Otherwise a typo in the name would write nothing, report success, and leave
// an operator believing the organisation was provisioned.
func TestSettingAConsoleForANonexistentOrgIsRefused(t *testing.T) {
	db := newTestStore(t)

	err := db.SetConsoleURL(context.Background(), "ghost", "https://ghost.example")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetConsoleURL returned %v, want ErrNotFound", err)
	}
}

// The database refuses a console URL that is not absolute.
//
// Deliberately going around SetConsoleURL, the same way
// TestTheDatabaseRefusesAnUnfoldedEmail goes around NormalizeEmail. The Go side
// validates too, and this asserts the two agree — a relative value would
// resolve against Cloud's own origin in a Location header and send a browser
// carrying an assertion to a Cloud path.
func TestTheDatabaseRefusesARelativeConsoleURL(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateOrg(ctx, "checked", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, bad := range []string{"acme.example", "/login", "https://", "ftp://acme.example"} {
		_, err := db.pool.Exec(ctx,
			`UPDATE cloud.orgs SET console_url = $2 WHERE name = $1`, "checked", bad)
		if err == nil {
			t.Errorf("the database accepted %q as a console URL", bad)
		}
	}

	// The empty string stays legal: it is the "not registered yet" state, and
	// ConsoleURL is what turns it into an error.
	if _, err := db.pool.Exec(ctx,
		`UPDATE cloud.orgs SET console_url = '' WHERE name = $1`, "checked"); err != nil {
		t.Errorf("the database refused the unregistered state: %v", err)
	}
}

// Deleting an organisation takes its console with it.
func TestDeletingAnOrgRemovesItsConsole(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	if err := db.CreateOrg(ctx, "gone", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.SetConsoleURL(ctx, "gone", "https://gone.example"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `DELETE FROM cloud.orgs WHERE name = 'gone'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.ConsoleURL(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsoleURL returned %v after the org was deleted, want ErrNotFound", err)
	}
}
