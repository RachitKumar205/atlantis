package console

// The fleet sweep against real organisations.
//
// The harness runs the real admin service over real mTLS against each
// organisation's own database, so these seed a tenant and read what the sweep
// made of it rather than asserting against a stub.
//
// Every gauge assertion lives in this file and these tests run sequentially.
// The collectors are package-level, so two tests asserting absolute series
// counts at once would interfere.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// seedTenant gives one organisation's database something for the sweep to
// count: a dead job, two parked objects of which one is past its retention, and
// a freeze window covering now.
func seedTenant(t *testing.T, f *consoleFixture) {
	t.Helper()
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO atlantis.jobs_dead (id, job_name, queue, args, attempts, max_retries, enqueued_at, moved_at)
		VALUES (1, 'send_email', 'default', '{}'::jsonb, 3, 3, NOW(), NOW())
	`); err != nil {
		t.Fatalf("seed a dead job: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO atlantis.parked_objects
		    (kind, schema_name, object_name, original_schema, original_name, parked_at, reap_after)
		VALUES ('table', 'atlantis_tombstone', 'old_orders', 'public', 'orders', NOW(), NOW() - interval '1 day'),
		       ('table', 'atlantis_tombstone', 'old_carts',  'public', 'carts',  NOW(), NOW() + interval '30 days')
	`); err != nil {
		t.Fatalf("seed parked objects: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `
		INSERT INTO atlantis.freeze_windows (starts_at, ends_at, display_tz, classes, reason, created_by)
		VALUES (NOW() - interval '1 hour', NOW() + interval '4 hours', 'UTC', ARRAY['']::text[], 'release', 'ops')
	`); err != nil {
		t.Fatalf("seed a freeze window: %v", err)
	}
}

func sweepOnce(t *testing.T, f *consoleFixture) *fleetPoller {
	t.Helper()
	p := &fleetPoller{published: map[string]bool{}}
	f.srv.runFleetPoll(context.Background(), p)
	return p
}

// readFacts reads one organisation's row as the superuser.
//
// Not through the store: a scoped read that returned the right numbers would
// also pass against a policy admitting nothing, which is the contract stated in
// org_isolation_pg_test.go.
type factRow struct {
	Reachable       bool
	UnreachableKind *string
	LastError       string
	CollectedAt     time.Time
	FactsAt         *time.Time
	SchemaVersion   *int64
	ServerVersion   string
	DeadJobs        *int32
	ParkedObjects   *int32
	ParkedOverdue   *int32
	FreezeOpen      *bool
	FreezeEndsAt    *time.Time
}

func readFacts(t *testing.T, f *consoleFixture, org string) (factRow, bool) {
	t.Helper()
	var r factRow
	err := f.pool.QueryRow(context.Background(), `
		SELECT reachable, unreachable_kind, last_error, collected_at, facts_at,
		       schema_version, server_version,
		       dead_jobs, parked_objects, parked_overdue, freeze_open, freeze_ends_at
		  FROM console.org_facts WHERE org = $1
	`, org).Scan(&r.Reachable, &r.UnreachableKind, &r.LastError, &r.CollectedAt, &r.FactsAt,
		&r.SchemaVersion, &r.ServerVersion,
		&r.DeadJobs, &r.ParkedObjects, &r.ParkedOverdue, &r.FreezeOpen, &r.FreezeEndsAt)
	if err != nil {
		return factRow{}, false
	}
	return r, true
}

// One sweep records what an organisation is running.
//
// The four facts a fleet view exists to answer, taken from a real tenant over
// the real admin plane.
func TestASweepRecordsEachOrganisationsFacts(t *testing.T) {
	f := newConsoleFixture(t)
	seedTenant(t, f)

	sweepOnce(t, f)

	got, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the sweep wrote no row for the organisation it visited")
	}
	if !got.Reachable {
		t.Fatalf("the organisation was reported unreachable: %s", got.LastError)
	}
	if got.FactsAt == nil {
		t.Error("facts_at was not set on a sweep that measured everything")
	}
	if got.ServerVersion == "" {
		t.Error("no server version, so /status was not read")
	}
	if got.DeadJobs == nil || *got.DeadJobs != 1 {
		t.Errorf("dead_jobs = %v, want 1", got.DeadJobs)
	}
	if got.ParkedObjects == nil || *got.ParkedObjects != 2 {
		t.Errorf("parked_objects = %v, want 2", got.ParkedObjects)
	}
	// The question the register is read for: how much should already have gone.
	if got.ParkedOverdue == nil || *got.ParkedOverdue != 1 {
		t.Errorf("parked_overdue = %v, want 1 — one object's reap_after has passed",
			got.ParkedOverdue)
	}
	if got.FreezeOpen == nil || !*got.FreezeOpen {
		t.Errorf("freeze_open = %v, want true", got.FreezeOpen)
	}

	if n := testutil.ToFloat64(fleetReachable.WithLabelValues(defaultOrg)); n != 1 {
		t.Errorf("the reachable gauge reads %v, want 1", n)
	}
	if n := testutil.ToFloat64(fleetDeadJobs.WithLabelValues(defaultOrg)); n != 1 {
		t.Errorf("the dead-jobs gauge reads %v, want 1", n)
	}
}

// An organisation that cannot be reached records why, and records no counts.
//
// NULL and zero are different answers. An organisation with no dead jobs and
// one whose admin plane could not be reached would both read as 0 in a NOT NULL
// column, and the second is the one worth acting on.
func TestAnUnreachableOrganisationRecordsNoCountsRatherThanZero(t *testing.T) {
	f := newConsoleFixture(t)

	// The tenant's pod goes away. Editing the registry row instead would not
	// reach the sweep: the client cache returns its entry for orgClientRefresh
	// without re-reading the row, which is what a deployment does too.
	f.atl.stopHealth()

	sweepOnce(t, f)

	got, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the sweep wrote no row for an organisation it could not reach")
	}
	if got.Reachable {
		t.Fatal("an organisation with nothing listening was reported reachable")
	}
	if got.DeadJobs != nil {
		t.Errorf("dead_jobs = %v on an unreachable organisation, want NULL — zero would "+
			"read as a healthy empty queue", *got.DeadJobs)
	}
	if got.FactsAt != nil {
		t.Error("facts_at was set by a sweep that measured nothing")
	}
	if got.LastError == "" {
		t.Error("no error text, so the row says it failed and not why")
	}
	if got.UnreachableKind == nil || *got.UnreachableKind != kindDial {
		t.Errorf("unreachable_kind = %v, want %q", got.UnreachableKind, kindDial)
	}
	if n := testutil.ToFloat64(fleetReachable.WithLabelValues(defaultOrg)); n != 0 {
		t.Errorf("the reachable gauge reads %v, want 0", n)
	}
}

// A sweep that fails keeps the numbers the last good sweep recorded.
//
// The alternative erases what was known the first time a tenant restarts, so an
// operator asking what an organisation was running learns nothing.
// A /status without schema_version is an unknown version, never 0: the stored
// version keeps its last value, and the gauge reads NaN.
func TestAMissingSchemaVersionIsUnknownNotZero(t *testing.T) {
	f := newConsoleFixture(t)
	seedTenant(t, f)

	f.atl.setStatus(func() (int, string) {
		return 200, `{"started_at":"2026-09-29T00:00:00Z","version":"test","schema_version":7}`
	})
	sweepOnce(t, f)
	f.atl.setStatus(func() (int, string) {
		return 200, `{"started_at":"2026-09-29T00:00:00Z","version":"test"}`
	})
	sweepOnce(t, f)

	got, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the sweep wrote no row")
	}
	if !got.Reachable {
		t.Fatalf("a tenant that answered /status was reported unreachable: %s", got.LastError)
	}
	if got.SchemaVersion == nil || *got.SchemaVersion != 7 {
		var stored any = "NULL"
		if got.SchemaVersion != nil {
			stored = *got.SchemaVersion
		}
		t.Errorf("schema_version = %v after a status without one, want the last known 7", stored)
	}
	if v := testutil.ToFloat64(fleetSchemaVersion.WithLabelValues(defaultOrg)); !math.IsNaN(v) {
		t.Errorf("the schema-version gauge reads %v for an unknown version, want NaN", v)
	}
}

func TestAFailedSweepKeepsTheLastKnownFacts(t *testing.T) {
	f := newConsoleFixture(t)
	seedTenant(t, f)

	sweepOnce(t, f)
	before, ok := readFacts(t, f, defaultOrg)
	if !ok || before.DeadJobs == nil {
		t.Fatal("the first sweep recorded nothing to keep")
	}
	firstFactsAt := before.FactsAt

	f.atl.stopHealth()
	sweepOnce(t, f)

	after, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the row was removed by a failed sweep")
	}
	if after.Reachable {
		t.Fatal("the second sweep reported the broken organisation reachable")
	}
	if after.DeadJobs == nil || *after.DeadJobs != *before.DeadJobs {
		t.Errorf("dead_jobs = %v after a failed sweep, want the last known %v",
			after.DeadJobs, *before.DeadJobs)
	}
	if after.FactsAt == nil || !after.FactsAt.Equal(*firstFactsAt) {
		t.Errorf("facts_at moved to %v on a sweep that measured nothing, want %v — "+
			"the two timestamps are what say how stale the numbers are",
			after.FactsAt, firstFactsAt)
	}
}

// An organisation that has gone stops being published.
//
// GaugeVec children are never collected, so an organisation removed from the
// registry would keep publishing its last values and an alert on it would never
// clear.
func TestAnOrganisationThatDisappearsStopsBeingPublished(t *testing.T) {
	f := newTwoOrgFixture(t)
	ctx := context.Background()

	p := &fleetPoller{published: map[string]bool{}}
	f.srv.runFleetPoll(ctx, p)

	if n := testutil.CollectAndCount(fleetReachable); n != 2 {
		t.Fatalf("the reachable gauge has %d children after a two-organisation sweep, want 2", n)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM console.orgs WHERE org = $1`, otherOrg); err != nil {
		t.Fatalf("remove the second organisation: %v", err)
	}
	f.srv.runFleetPoll(ctx, p)

	if n := testutil.CollectAndCount(fleetReachable); n != 1 {
		t.Errorf("the reachable gauge has %d children after one organisation was removed, "+
			"want 1 — a purged organisation keeps alerting for ever", n)
	}
	if n := testutil.ToFloat64(fleetReachable.WithLabelValues(defaultOrg)); n != 1 {
		t.Errorf("the surviving organisation reads %v, want 1; retiring took the wrong child", n)
	}
}

// A sweep with no organisations publishes zero rather than the last count.
func TestASweepOfAnEmptyFleetPublishesZero(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	p := &fleetPoller{published: map[string]bool{}}
	f.srv.runFleetPoll(ctx, p)
	if n := testutil.ToFloat64(fleetOrgs); n != 1 {
		t.Fatalf("fleet size reads %v after one organisation, want 1", n)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM console.orgs`); err != nil {
		t.Fatalf("empty the registry: %v", err)
	}
	f.srv.runFleetPoll(ctx, p)

	if n := testutil.ToFloat64(fleetOrgs); n != 0 {
		t.Errorf("fleet size reads %v with no organisations left, want 0", n)
	}
	if n := testutil.CollectAndCount(fleetReachable); n != 0 {
		t.Errorf("%d organisations are still published after the registry was emptied", n)
	}
}

// A tenant that accepts connections and never answers does not consume the
// sweep.
//
// This is the test that fails if the per-organisation deadline is removed.
// Nothing else in the package sets a deadline on an outbound call, so without
// one a wedged tenant holds its worker until the whole sweep's budget runs out.
func TestAWedgedTenantDoesNotConsumeTheSweep(t *testing.T) {
	f := newTwoOrgFixture(t)

	// The default organisation's health listener accepts and stalls well past
	// the budget below.
	f.atl.setStatus(func() (int, string) {
		time.Sleep(10 * time.Second)
		return 200, "{}"
	})
	f.srv.cfg.FleetOrgTimeout = 300 * time.Millisecond

	started := time.Now()
	p := &fleetPoller{published: map[string]bool{}}
	f.srv.runFleetPoll(context.Background(), p)
	elapsed := time.Since(started)

	// Well inside healthTimeout, the health client's own five seconds. A bound
	// looser than that would be satisfied by the client's timeout and would say
	// nothing about the sweep's own deadline — which is the thing under test,
	// since nothing else in this package sets one.
	if elapsed > 2*time.Second {
		t.Errorf("the sweep took %s against one stalled tenant with a %s per-organisation "+
			"budget; the sweep's own deadline is not bounding it",
			elapsed, f.srv.cfg.FleetOrgTimeout)
	}

	// The wedged one is recorded as unreachable, and the healthy one is
	// complete — the point of bounding one organisation is that the rest still
	// report.
	stalled, ok := readFacts(t, f.consoleFixture, defaultOrg)
	if !ok {
		t.Fatal("no row for the stalled organisation")
	}
	if stalled.Reachable {
		t.Error("an organisation that never answered was reported reachable")
	}
	if stalled.UnreachableKind == nil || *stalled.UnreachableKind != kindTimeout {
		t.Errorf("unreachable_kind = %v, want %q", stalled.UnreachableKind, kindTimeout)
	}

	healthy, ok := readFacts(t, f.consoleFixture, otherOrg)
	if !ok {
		t.Fatal("no row for the organisation that was answering")
	}
	if !healthy.Reachable {
		t.Errorf("the healthy organisation was dragged down with the wedged one: %s",
			healthy.LastError)
	}
}

// A sweep that reads /status but none of the facts does not claim they are
// current.
//
// facts_at is the staleness signal: the gap between it and collected_at says
// how old the numbers are. A tenant whose health listener answers and whose
// admin plane does not is the shape a mixed-version fleet produces, and on it
// the numbers kept from the last good sweep are exactly as old as that sweep.
func TestAPartialSweepDoesNotClaimTheFactsAreCurrent(t *testing.T) {
	f := newConsoleFixture(t)
	seedTenant(t, f)

	sweepOnce(t, f)
	first, ok := readFacts(t, f, defaultOrg)
	if !ok || first.FactsAt == nil || first.DeadJobs == nil {
		t.Fatal("setup: the first sweep did not record a complete measurement")
	}

	// /status keeps answering; the admin plane goes.
	f.atl.stopGRPC()
	time.Sleep(10 * time.Millisecond)
	sweepOnce(t, f)

	got, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the partial sweep removed the row")
	}
	if !got.Reachable {
		t.Fatal("the health listener answered, so the organisation is reachable")
	}
	if !got.CollectedAt.After(first.CollectedAt) {
		t.Fatal("setup: collected_at did not move, so the second sweep did not write")
	}
	if got.FactsAt == nil || !got.FactsAt.Equal(*first.FactsAt) {
		t.Errorf("facts_at moved to %v on a sweep that read none of the facts, want "+
			"the first sweep's %v — the numbers in the row are that old, and the "+
			"row now claims otherwise", got.FactsAt, first.FactsAt)
	}
	if got.DeadJobs == nil || *got.DeadJobs != *first.DeadJobs {
		t.Errorf("dead_jobs = %v, want the last known %d kept", got.DeadJobs, *first.DeadJobs)
	}
	if got.LastError == "" {
		t.Error("the facts that could not be read left no error text")
	}
}

// A freeze that ends clears the recorded end.
//
// The one piece of SQL here that is not a COALESCE: freeze_ends_at has to go
// back to NULL when a freeze lifts, and keep its value when the freeze could not
// be read. A COALESCE would leave a lifted freeze reporting when it ends.
func TestAFreezeThatLiftsClearsItsEnd(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()
	seedTenant(t, f)

	sweepOnce(t, f)
	open, ok := readFacts(t, f, defaultOrg)
	if !ok || open.FreezeOpen == nil || !*open.FreezeOpen {
		t.Fatal("setup: the seeded freeze was not recorded as open")
	}
	if open.FreezeEndsAt == nil {
		t.Fatal("an open freeze recorded no end, so nobody can tell when it lifts")
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM atlantis.freeze_windows`); err != nil {
		t.Fatalf("lift the freeze: %v", err)
	}
	sweepOnce(t, f)

	lifted, ok := readFacts(t, f, defaultOrg)
	if !ok {
		t.Fatal("the row went away")
	}
	if lifted.FreezeOpen == nil || *lifted.FreezeOpen {
		t.Errorf("freeze_open = %v after the freeze was lifted, want false", lifted.FreezeOpen)
	}
	if lifted.FreezeEndsAt != nil {
		t.Errorf("freeze_ends_at = %v after the freeze lifted, want NULL — a lifted "+
			"freeze still says when it ends", *lifted.FreezeEndsAt)
	}
}

// Every column survives the round trip through the store.
//
// The read-back scans sixteen columns by position. The counts carry distinct
// values so a transposition — parked_objects read into parked_overdue — fails
// here rather than reporting one number as another.
func TestFleetFactsRoundTripThroughTheStore(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	collected := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	started := collected.Add(-time.Hour)
	endsAt := collected.Add(4 * time.Hour)
	version, dead, parked, overdue := int64(42), int32(3), int32(5), int32(7)
	yes, no := true, false

	want := orgFacts{
		Org: defaultOrg, CollectedAt: collected,
		Reachable: true, LastError: "freeze windows: something",
		SchemaVersion: &version, ServerVersion: "d887869", StartedAt: &started,
		DeadJobs: &dead, DeadJobsTruncated: &no,
		ParkedObjects: &parked, ParkedOverdue: &overdue, ParkedTruncated: &yes,
		FreezeOpen: &yes, FreezeEndsAt: &endsAt,
		MeasuredFacts: true,
	}
	if err := f.srv.db.forOrg(defaultOrg).upsertOrgFacts(ctx, want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, found, err := f.srv.db.forOrg(defaultOrg).orgFacts(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !found {
		t.Fatal("no row read back")
	}

	check := func(what string, g, w any) {
		t.Helper()
		if g != w {
			t.Errorf("%s = %v, want %v", what, g, w)
		}
	}
	check("org", got.Org, want.Org)
	check("reachable", got.Reachable, want.Reachable)
	check("last_error", got.LastError, want.LastError)
	check("server_version", got.ServerVersion, want.ServerVersion)
	check("measured", got.MeasuredFacts, want.MeasuredFacts)
	check("schema_version", *got.SchemaVersion, version)
	check("dead_jobs", *got.DeadJobs, dead)
	check("dead_jobs_truncated", *got.DeadJobsTruncated, no)
	check("parked_objects", *got.ParkedObjects, parked)
	check("parked_overdue", *got.ParkedOverdue, overdue)
	check("parked_truncated", *got.ParkedTruncated, yes)
	check("freeze_open", *got.FreezeOpen, yes)
	if !got.CollectedAt.Equal(collected) {
		t.Errorf("collected_at = %v, want %v", got.CollectedAt, collected)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
	}
	if got.FreezeEndsAt == nil || !got.FreezeEndsAt.Equal(endsAt) {
		t.Errorf("freeze_ends_at = %v, want %v", got.FreezeEndsAt, endsAt)
	}
}
