package provisioner

// Image drift and the rolls that clear it.
//
// Two properties, and they pull against each other. Nothing may roll an
// organisation without an operator's request, because a control-plane deploy
// carrying a new Postgres image would otherwise restart every customer's
// database. And a request must reach the workload, because the state these
// tests exist for is the one the fleet was found in: three organisations on
// three different server images with nothing reporting it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/console"
)

// wantImages is what testConfig's provisioner is configured with.
func wantImages() provision.Images {
	return provision.Images{
		Server:   "atlantis-server:local",
		Signer:   "atlantis-signer:local",
		Postgres: "atlantis-pg:17.11",
	}
}

func rollWorker(t *testing.T, q *fakeQueue, c *fakeCluster) *Worker {
	t.Helper()
	c.configured = wantImages()
	return newTestWorker(t, testConfig(), q, c,
		func(context.Context, string, string, console.OrgRegistration) error { return nil })
}

func drift(kind provision.ImageKind) float64 {
	return testutil.ToFloat64(tenantImageDrift.WithLabelValues(string(kind)))
}

// rolledAudits returns the detail of every org.images_rolled row written.
func rolledAudits(q *fakeQueue) []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []map[string]any
	for _, a := range q.audits {
		if a.action == "org.images_rolled" {
			out = append(out, a.detail)
		}
	}
	return out
}

// An organisation on an old image is counted, and nothing rolls it.
//
// The counting and the not-rolling are asserted together on purpose: a pass
// that rolled would also clear the drift, so a test that only read the gauge
// afterwards could not tell "reported" from "silently fixed".
func TestDriftIsReportedAndNotCorrected(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"stale", "current"}
	c := &fakeCluster{images: map[string]provision.Images{
		"stale": {
			Server:   "atlantis-server:7c9f3ad",
			Signer:   "atlantis-signer:local",
			Postgres: "atlantis-pg:17.11",
		},
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 1 {
		t.Errorf("server drift = %v, want 1 — one organisation is on 7c9f3ad", got)
	}
	for _, kind := range []provision.ImageKind{provision.ImageSigner, provision.ImagePostgres} {
		if got := drift(kind); got != 0 {
			t.Errorf("%s drift = %v, want 0; only the server image differs", kind, got)
		}
	}

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()
	if len(rolls) != 0 {
		t.Fatalf("reconcile rolled an organisation nobody asked it to: %+v.\n"+
			"A deploy carrying a new Postgres image would restart every customer's "+
			"database on the next pass", rolls)
	}
}

// A missing workload counts as drift rather than as agreement.
//
// Exists reads the namespace, which survives a Deployment somebody deleted, so
// this is the one signal that would report it. An empty image compared as equal
// to nothing would make a deleted server look like a converged one.
func TestAnAbsentWorkloadCountsAsDrift(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"gutted"}
	c := &fakeCluster{images: map[string]provision.Images{
		"gutted": {Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 1 {
		t.Errorf("server drift = %v, want 1 — the Deployment is gone", got)
	}
}

// An organisation whose images could not be read is left out of the tally, and
// the rest of the pass still reports.
//
// The alternative is a pass that reports nothing because one organisation's
// namespace answered slowly.
func TestAnUnreadableOrganisationDoesNotStopTheTally(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"unreadable", "stale"}
	c := &fakeCluster{
		imagesErr: map[string]error{"unreadable": errors.New("api server timeout")},
		images: map[string]provision.Images{
			"stale": {Server: "atlantis-server:old", Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
		},
	}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 1 {
		t.Errorf("server drift = %v, want 1 — the readable organisation is stale "+
			"and the unreadable one is not counted either way", got)
	}
}

// A request rolls exactly the kinds it names, and is cleared against the
// timestamp the pass observed.
func TestARequestedRollAppliesTheNamedKindsAndClears(t *testing.T) {
	asked := time.Now().Add(-time.Minute)
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rollRequests = map[string]store.ImageRoll{
		"asked": {Kinds: []string{"server", "signer"}, RequestedAt: asked},
	}
	c := &fakeCluster{images: map[string]provision.Images{
		"asked": {Server: "atlantis-server:old", Signer: "atlantis-signer:old", Postgres: "atlantis-pg:old"},
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	got := c.images["asked"]
	c.mu.Unlock()

	if len(rolls) != 1 || rolls[0].org != "asked" {
		t.Fatalf("the request did not roll the organisation: %+v", rolls)
	}
	if len(rolls[0].kinds) != 2 {
		t.Fatalf("rolled %v, want exactly the two kinds asked for", rolls[0].kinds)
	}
	// Postgres was not asked for, so the database must not have been touched.
	// This is the assertion that keeps `cloud org roll` without -postgres from
	// restarting a customer's database.
	if got.Postgres != "atlantis-pg:old" {
		t.Errorf("postgres moved to %q on a request that named server and signer; "+
			"the database restarted for a roll nobody asked for", got.Postgres)
	}
	if got.Server != wantImages().Server || got.Signer != wantImages().Signer {
		t.Errorf("images after the roll = %+v, want the configured server and signer", got)
	}

	q.mu.Lock()
	cleared := append([]clearedRoll(nil), q.rollCleared...)
	q.mu.Unlock()
	if len(cleared) != 1 || cleared[0].org != "asked" {
		t.Fatalf("the request was not cleared, so it would roll again every pass: %+v", cleared)
	}
	if !cleared[0].seen.Equal(asked) {
		t.Errorf("cleared against %s, want the observed request time %s — a request "+
			"made while the roll ran would be discarded", cleared[0].seen, asked)
	}
}

// A roll that failed leaves the request outstanding.
//
// Half an organisation's workloads carrying the new image and half the old one
// is the state this is for. Clearing would report the roll done and leave the
// drift gauge as the only sign, five minutes later.
func TestAFailedRollLeavesTheRequestOutstanding(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rollRequests = map[string]store.ImageRoll{
		"asked": {Kinds: []string{"server"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{rollErr: map[string]error{"asked": errors.New("admission webhook refused")}}
	w := rollWorker(t, q, c)

	failedBefore := testutil.ToFloat64(imageRollsTotal.WithLabelValues("server", "failed"))

	w.reconcile(context.Background())

	q.mu.Lock()
	cleared := append([]clearedRoll(nil), q.rollCleared...)
	q.mu.Unlock()
	if len(cleared) != 0 {
		t.Error("a roll that failed cleared its request; the next pass would not retry it")
	}
	if got := testutil.ToFloat64(imageRollsTotal.WithLabelValues("server", "failed")); got != failedBefore+1 {
		t.Errorf("failed rolls = %v, want %v", got, failedBefore+1)
	}
}

// The per-pass cap holds, and the oldest request goes first.
//
// The cap is what makes `cloud org roll -all -postgres` survivable: one database
// down at a time rather than the whole fleet. Oldest first so a request made
// later cannot keep an earlier one waiting for ever.
func TestRollsAreCappedPerPassOldestFirst(t *testing.T) {
	now := time.Now()
	q := newFakeQueue()
	q.ready = []string{"third", "first", "second"}
	q.rollRequests = map[string]store.ImageRoll{
		"third":  {Kinds: []string{"server"}, RequestedAt: now},
		"first":  {Kinds: []string{"server"}, RequestedAt: now.Add(-2 * time.Hour)},
		"second": {Kinds: []string{"server"}, RequestedAt: now.Add(-time.Hour)},
	}
	c := &fakeCluster{}
	c.configured = wantImages()

	cfg := testConfig()
	cfg.MaxRollsPerPass = 2
	w := newTestWorker(t, cfg, q, c,
		func(context.Context, string, string, console.OrgRegistration) error { return nil })

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()

	if len(rolls) != 2 {
		t.Fatalf("rolled %d organisations, want the cap of 2: %+v", len(rolls), rolls)
	}
	if rolls[0].org != "first" || rolls[1].org != "second" {
		t.Errorf("rolled %s then %s, want first then second — oldest request first",
			rolls[0].org, rolls[1].org)
	}
}

// A request against an organisation the cluster has lost is not acted on.
//
// Requeueing is what is happening to it, and rolling would apply workloads into
// a namespace that is being rebuilt from scratch.
func TestAnAbsentOrganisationIsNotRolled(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"gone"}
	q.rollRequests = map[string]store.ImageRoll{
		"gone": {Kinds: []string{"server"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{absent: map[string]bool{"gone": true}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()
	if len(rolls) != 0 {
		t.Errorf("rolled an organisation the cluster no longer has: %+v", rolls)
	}

	q.mu.Lock()
	cleared := append([]clearedRoll(nil), q.rollCleared...)
	q.mu.Unlock()
	if len(cleared) != 0 {
		t.Error("cleared the request for an organisation that was never rolled")
	}
}

// A request naming only kinds this build does not have is cleared, not retried.
//
// Retrying would re-read the same row every five minutes for ever and log the
// same error. Clearing it costs an operator one re-run of a command that named
// something wrong.
func TestARequestWithNoKnownKindIsClearedRatherThanRepeated(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"odd"}
	q.rollRequests = map[string]store.ImageRoll{
		"odd": {Kinds: []string{"memcached"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()
	if len(rolls) != 0 {
		t.Errorf("rolled something for a kind this build does not have: %+v", rolls)
	}

	q.mu.Lock()
	cleared := append([]clearedRoll(nil), q.rollCleared...)
	q.mu.Unlock()
	if len(cleared) != 1 {
		t.Error("the request was left outstanding, so every pass repeats the same error")
	}
}

// A request naming one known kind beside an unknown one rolls the known one.
func TestAnUnknownKindDoesNotBlockTheRest(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"mixed"}
	q.rollRequests = map[string]store.ImageRoll{
		"mixed": {Kinds: []string{"memcached", "server"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()
	if len(rolls) != 1 || len(rolls[0].kinds) != 1 || rolls[0].kinds[0] != provision.ImageServer {
		t.Fatalf("rolled %+v, want the server kind alone", rolls)
	}
}

// Requests that cannot be read do not stop the rest of the pass.
//
// Same terms as the console rotation requests beside them: existence checking
// and drift reporting still run, and the request is honoured next interval.
func TestReconcileStillReportsDriftWhenRollRequestsCannotBeRead(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"stale"}
	q.rollRequestsErr = errors.New("cloud database is down")
	c := &fakeCluster{images: map[string]provision.Images{
		"stale": {Server: "atlantis-server:old", Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 1 {
		t.Errorf("server drift = %v, want 1; a failed request read stopped the tally", got)
	}
}

// One roll that can never succeed does not hold the queue.
//
// The failure mode this exists for: with the cap at one and ordering on age
// alone, an organisation whose apply is always refused keeps the oldest
// timestamp, takes the only slot on every pass, and no other organisation in
// the fleet is ever rolled. The one visible signal is a throttling log line
// that reads as normal.
func TestAPermanentlyFailingRollDoesNotBlockTheQueue(t *testing.T) {
	now := time.Now()
	q := newFakeQueue()
	q.ready = []string{"wedged", "waiting"}
	q.rollRequests = map[string]store.ImageRoll{
		// Oldest, so it sorts first on the first pass.
		"wedged":  {Kinds: []string{"server"}, RequestedAt: now.Add(-time.Hour)},
		"waiting": {Kinds: []string{"server"}, RequestedAt: now},
	}
	c := &fakeCluster{rollErr: map[string]error{
		"wedged": errors.New("admission webhook refuses to shrink storage"),
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())
	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	c.mu.Unlock()

	if len(rolls) != 2 {
		t.Fatalf("two passes produced %d rolls, want one each: %+v", len(rolls), rolls)
	}
	if rolls[0].org != "wedged" {
		t.Errorf("the first pass rolled %s, want the oldest request", rolls[0].org)
	}
	if rolls[1].org != "waiting" {
		t.Errorf("the second pass rolled %s again, want waiting.\n"+
			"A request that cannot succeed is holding the only slot, so no other "+
			"organisation in the fleet will ever roll", rolls[1].org)
	}
}

// A failed roll is counted, and counted against the request that was seen.
func TestAFailedRollIsCountedAgainstTheObservedRequest(t *testing.T) {
	asked := time.Now().Add(-time.Minute)
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rollRequests = map[string]store.ImageRoll{
		"asked": {Kinds: []string{"server"}, RequestedAt: asked},
	}
	c := &fakeCluster{rollErr: map[string]error{"asked": errors.New("refused")}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	q.mu.Lock()
	failures := append([]clearedRoll(nil), q.rollFailures...)
	q.mu.Unlock()
	if len(failures) != 1 || failures[0].org != "asked" {
		t.Fatalf("the failure was not counted, so the request keeps its place at the "+
			"head of the queue for ever: %+v", failures)
	}
	if !failures[0].seen.Equal(asked) {
		t.Errorf("counted against %s, want the observed request time %s — a fresh "+
			"request would inherit the failure count of the one it replaced",
			failures[0].seen, asked)
	}
}

// A roll whose images did not move is not reported as having moved them.
//
// PROVISIONER_POSTGRES_IMAGE carries a version tag, not a digest, so rebuilding
// and pushing that tag leaves the configured string unchanged. Re-applying then
// writes the same object and starts no pod. Recording the configured image as
// what was rolled would put a restart in the audit log that never happened.
func TestARollThatChangedNothingSaysSo(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"current"}
	q.rollRequests = map[string]store.ImageRoll{
		"current": {Kinds: []string{"server"}, RequestedAt: time.Now()},
	}
	// No images entry, so the fake reports the configured set already in use.
	c := &fakeCluster{}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	details := rolledAudits(q)
	if len(details) != 1 {
		t.Fatalf("the roll wrote %d audit rows, want one", len(details))
	}
	if got := details[0]["changed"]; got != "" {
		t.Errorf("changed = %q, want empty — nothing moved, so nothing restarted", got)
	}
	if got := details[0]["unchanged"]; got != "server" {
		t.Errorf("unchanged = %q, want \"server\"", got)
	}
}

// A roll that did move an image records what the cluster reports afterwards.
func TestARollRecordsWhatTheClusterRunsAfterwards(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"stale"}
	q.rollRequests = map[string]store.ImageRoll{
		"stale": {Kinds: []string{"server"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{images: map[string]provision.Images{
		"stale": {Server: "atlantis-server:old", Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
	}}

	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	details := rolledAudits(q)
	if len(details) != 1 {
		t.Fatalf("the roll wrote %d audit rows, want one", len(details))
	}
	want := "server=" + wantImages().Server
	if got := details[0]["changed"]; got != want {
		t.Errorf("changed = %q, want %q", got, want)
	}
}

// The postgres kind reaches the cluster, and only when it was asked for.
//
// Every other roll test names server or signer, so the expensive kind — the one
// that restarts a customer's database — would otherwise have no positive case.
func TestAPostgresRequestRollsPostgres(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rollRequests = map[string]store.ImageRoll{
		"asked": {Kinds: []string{"postgres"}, RequestedAt: time.Now()},
	}
	c := &fakeCluster{images: map[string]provision.Images{
		"asked": {Server: "atlantis-server:old", Signer: "atlantis-signer:old", Postgres: "atlantis-pg:old"},
	}}
	w := rollWorker(t, q, c)

	w.reconcile(context.Background())

	c.mu.Lock()
	rolls := append([]rollCall(nil), c.rollCalls...)
	got := c.images["asked"]
	c.mu.Unlock()

	if len(rolls) != 1 || len(rolls[0].kinds) != 1 || rolls[0].kinds[0] != provision.ImagePostgres {
		t.Fatalf("rolled %+v, want the postgres kind alone", rolls)
	}
	if got.Postgres != wantImages().Postgres {
		t.Errorf("postgres = %q, want the configured image", got.Postgres)
	}
	if got.Server != "atlantis-server:old" {
		t.Errorf("server moved to %q on a postgres-only request", got.Server)
	}
}

// A pass that could read no organisation's images leaves the gauges alone.
//
// Zero here reads as "every organisation is current", and the documented alert
// is an equality against the organisation count — so publishing zero after a
// pass that measured nothing reports health. The console certificate gauge
// beside it takes the same decision for the same reason.
func TestAPassThatMeasuredNothingLeavesTheDriftGaugesAlone(t *testing.T) {
	// A first pass establishes a known value.
	q := newFakeQueue()
	q.ready = []string{"stale"}
	c := &fakeCluster{images: map[string]provision.Images{
		"stale": {Server: "atlantis-server:old", Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
	}}
	w := rollWorker(t, q, c)
	w.reconcile(context.Background())
	if got := drift(provision.ImageServer); got != 1 {
		t.Fatalf("setup: server drift = %v, want 1", got)
	}

	// Now a pass where every read fails.
	q2 := newFakeQueue()
	q2.ready = []string{"stale"}
	c2 := &fakeCluster{imagesErr: map[string]error{"stale": errors.New("api server timeout")}}
	w2 := rollWorker(t, q2, c2)
	w2.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 1 {
		t.Errorf("server drift = %v after a pass that measured nothing, want the "+
			"previous 1. Zero reads as a converged fleet and silences the alert", got)
	}
}

// An empty fleet publishes zero rather than holding a stale value.
//
// The guard above is for a pass that failed to measure, not for one with
// nothing to measure. An organisation purged while drifted would otherwise
// leave its contribution in the gauge for ever.
func TestAnEmptyFleetPublishesZeroDrift(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"stale"}
	c := &fakeCluster{images: map[string]provision.Images{
		"stale": {Server: "atlantis-server:old", Signer: "atlantis-signer:local", Postgres: "atlantis-pg:17.11"},
	}}
	w := rollWorker(t, q, c)
	w.reconcile(context.Background())
	if got := drift(provision.ImageServer); got != 1 {
		t.Fatalf("setup: server drift = %v, want 1", got)
	}

	q2 := newFakeQueue()
	q2.ready = nil
	w2 := rollWorker(t, q2, &fakeCluster{})
	w2.reconcile(context.Background())

	if got := drift(provision.ImageServer); got != 0 {
		t.Errorf("server drift = %v with no organisations left, want 0", got)
	}
}

// Every kind the CLI can name is one the provisioner can roll.
//
// The two halves are wired through a text column, so a kind added to one side
// and not the other is a request written and never acted on.
func TestEveryImageKindParsesBack(t *testing.T) {
	for _, kind := range provision.AllImageKinds {
		got, err := provision.ParseImageKind(string(kind))
		if err != nil {
			t.Errorf("ParseImageKind(%q): %v", kind, err)
			continue
		}
		if got != kind {
			t.Errorf("ParseImageKind(%q) = %q", kind, got)
		}
	}
	if _, err := provision.ParseImageKind("postgresql"); err == nil {
		t.Error("a near-miss kind parsed; a typo in a command would roll nothing and say nothing")
	}
}
