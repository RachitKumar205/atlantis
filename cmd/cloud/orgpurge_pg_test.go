package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// `cloud org purge`, the operator's escape hatch.
//
// Four flags and one UPDATE, but what these tests hold still is the guard, not
// the SQL. The command destroys a customer's database, their certificate
// authority and every schema in it, with no backup and no retention window in
// front of it, and `-yes` is the only thing between a recalled shell-history
// entry and that outcome.

func purgeTestDSN(t *testing.T) string {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise `cloud org purge`")
	}
	const dbName = "atlantis_cloud_purge"
	adminOnDB := pgcatalog.PrivateDatabase(t, adminDSN, dbName)
	return pgcatalog.IsolatedRoleDSN(t, adminOnDB, dbName, "cloud_purge_probe")
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedReadyOrg puts one serving organisation in the database.
//
// Ready rather than pending, because purge is documented as working from any
// state and `ready` is the one where getting it wrong costs the most — an
// operator purging an organisation that is still serving customers.
func seedReadyOrg(t *testing.T, dsn, org string) *store.Store {
	t.Helper()
	ctx := context.Background()

	if err := store.Migrate(dsn, quietLog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := store.New(ctx, dsn, quietLog())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)

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
	return db
}

// purgeAfterOf reads the reaper's trigger, or reports that it is unset.
func purgeAfterOf(t *testing.T, db *store.Store, org string) (set bool) {
	t.Helper()
	var after *string
	err := db.Pool().QueryRow(context.Background(),
		`SELECT purge_after::text FROM cloud.org_provisioning WHERE org = $1`, org).Scan(&after)
	if err != nil {
		t.Fatalf("read purge_after for %s: %v", org, err)
	}
	return after != nil
}

// Without -yes it must refuse, and refuse without writing anything.
//
// Both halves matter. A command that refused but had already set purge_after
// would be worse than one that did neither, because the refusal would say the
// organisation is safe while the reaper took it five minutes later.
func TestOrgPurgeRefusesWithoutYes(t *testing.T) {
	dsn := purgeTestDSN(t)
	db := seedReadyOrg(t, dsn, "noyes")

	err := orgPurge([]string{"-org", "noyes", "-db", dsn}, quietLog())
	if err == nil {
		t.Fatal("purge succeeded without -yes")
	}
	if !strings.Contains(err.Error(), "-yes") {
		t.Errorf("the refusal does not mention -yes, so it does not say how to "+
			"proceed: %v", err)
	}
	// The message has to say what is at stake. An operator who reads "refusing
	// without -yes" and nothing else learns only that a flag is missing.
	if !strings.Contains(err.Error(), "backups") {
		t.Errorf("the refusal does not mention that there are no backups: %v", err)
	}

	if purgeAfterOf(t, db, "noyes") {
		t.Fatal("purge_after was set despite the refusal: the reaper would " +
			"destroy an organisation the operator was told was safe")
	}
	p, err := db.ProvisioningFor(context.Background(), "noyes")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != store.StateReady {
		t.Fatalf("state is %q after a refused purge, want ready", p.State)
	}
}

// An organisation that does not exist must be reported, not silently accepted.
//
// The guarantee comes from store.PurgeNow, which returns ErrNotFound when its
// UPDATE matches no row — not from the read the command does first. That was
// worth establishing rather than assuming: removing the read-before-write
// entirely leaves this test passing, because PurgeNow's error already names the
// organisation.
//
// So the read is not a guard, and the comment on it should not read like one.
// What it buys is the printed line below, which TestOrgPurgeSaysWhatItIsAbout
// covers.
func TestOrgPurgeReportsAnUnknownOrganisation(t *testing.T) {
	dsn := purgeTestDSN(t)
	seedReadyOrg(t, dsn, "realone")

	err := orgPurge([]string{"-org", "typo-not-real", "-yes", "-db", dsn}, quietLog())
	if err == nil {
		t.Fatal("purging an organisation that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "typo-not-real") {
		t.Errorf("the error does not name what was not found: %v", err)
	}
}

func TestOrgPurgeRequiresAnOrganisationName(t *testing.T) {
	dsn := purgeTestDSN(t)
	if err := orgPurge([]string{"-yes", "-db", dsn}, quietLog()); err == nil {
		t.Fatal("purge with no -org succeeded")
	}
}

// The one that does the work: -yes arms the reaper.
//
// Asserted as "purge_after is now or in the past" rather than as an exact
// value, because the point is not the timestamp — it is that
// store.ClaimForPurge's predicate will now match this row. A test comparing
// against time.Now() would be asserting the clock.
func TestOrgPurgeArmsTheReaper(t *testing.T) {
	dsn := purgeTestDSN(t)
	db := seedReadyOrg(t, dsn, "armed")

	if err := orgPurge([]string{"-org", "armed", "-yes", "-db", dsn}, quietLog()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	p, err := db.ProvisioningFor(context.Background(), "armed")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != store.StateDeleted {
		t.Fatalf("state is %q after purge, want deleted", p.State)
	}

	// The real assertion: the reaper can now claim it. Reading the predicate's
	// answer rather than the column it reads, because the two agreeing is the
	// property — a purge_after that is set but that ClaimForPurge does not match
	// is an organisation queued for destruction that never gets destroyed.
	c, err := db.ClaimForPurge(context.Background(), "test-reaper", 60_000_000_000)
	if err != nil {
		t.Fatalf("the reaper could not claim an organisation that `cloud org purge` "+
			"had just armed: %v", err)
	}
	if c.Org != "armed" {
		t.Fatalf("the reaper claimed %q, want armed", c.Org)
	}
}

// Purging a serving organisation works, and that is deliberate.
//
// The store refuses a *user* deleting anything that is not ready; this refuses
// nothing, because an operator reaching for it is usually doing so precisely
// when the ordinary path will not do — a customer demanding erasure now, or an
// organisation wedged in a state the browser cannot act on.
func TestOrgPurgeWorksFromAnyState(t *testing.T) {
	dsn := purgeTestDSN(t)
	db := seedReadyOrg(t, dsn, "anystate")
	ctx := context.Background()

	for _, state := range []store.ProvisioningState{
		store.StatePending, store.StateProvisioning, store.StateFailed, store.StateReady,
	} {
		if _, err := db.Pool().Exec(ctx,
			`UPDATE cloud.org_provisioning SET state = $2, purge_after = NULL WHERE org = $1`,
			"anystate", string(state)); err != nil {
			t.Fatal(err)
		}
		if err := orgPurge([]string{"-org", "anystate", "-yes", "-db", dsn}, quietLog()); err != nil {
			t.Fatalf("purge from %s: %v", state, err)
		}
		if !purgeAfterOf(t, db, "anystate") {
			t.Fatalf("purge from %s left purge_after unset", state)
		}
	}
}

// A guard against the fixture rotting: the DSN really is isolated.
//
// Every test here writes to one shared private database, and seedReadyOrg would
// happily reuse an organisation left by an earlier run. Naming a distinct
// organisation per test is what keeps them independent; this checks the pool is
// actually pointed somewhere other than the developer's own database.
func TestThePurgeFixtureIsIsolated(t *testing.T) {
	dsn := purgeTestDSN(t)
	if !strings.Contains(dsn, "atlantis_cloud_purge") {
		t.Fatalf("the fixture DSN is %q, which is not the private database — these "+
			"tests destroy organisations", dsn)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var db string
	if err := pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	if db != "atlantis_cloud_purge" {
		t.Fatalf("connected to %q, want atlantis_cloud_purge", db)
	}
}

// The command says what it is about to destroy, and says it before it writes.
//
// Added after a mutation showed the read-before-write was untested: deleting it
// broke nothing, because the not-found guarantee lives in PurgeNow. What the
// read actually buys is this line — an operator sees which organisation and
// what state it was in, which is the difference between "purging acme
// (currently ready)" and a silent success.
//
// Untested, it is a line somebody removes while tidying, and the loss shows up
// only when a colleague purges the wrong thing and has no record of what state
// it was in.
func TestOrgPurgeSaysWhatItIsAbout(t *testing.T) {
	dsn := purgeTestDSN(t)
	seedReadyOrg(t, dsn, "loudpurge")

	out := captureStdout(t, func() {
		if err := orgPurge([]string{"-org", "loudpurge", "-yes", "-db", dsn}, quietLog()); err != nil {
			t.Fatalf("purge: %v", err)
		}
	})

	// "currently ready", not "ready" and not "loudpurge" separately.
	//
	// The looser assertions were tried first and both passed against a build
	// with the line deleted: the organisation's name appears in "X is queued for
	// destruction", and the word "ready" appears in the recovery SQL the command
	// prints afterwards. Two substrings that are present for other reasons are
	// not a test of the line that is supposed to carry them.
	//
	// "currently" appears nowhere else in the output.
	if !strings.Contains(out, "purging loudpurge (currently ready)") {
		t.Errorf("the output does not say which organisation is being destroyed and "+
			"what state it was in, so there is no record of what was purged: %q", out)
	}
}

// captureStdout collects what fn prints.
//
// os.Stdout is swapped for a pipe rather than the command being refactored to
// take an io.Writer. The printing is the behaviour under test and threading a
// writer through would change the thing being checked to suit the check.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	// Restored on the way out even if fn panics, because leaving os.Stdout as a
	// closed pipe would take every later test's output with it.
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}
