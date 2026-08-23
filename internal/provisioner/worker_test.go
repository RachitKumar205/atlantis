package provisioner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/console"
)

// The provisioning loop, tested without a cluster or a database.
//
// What a fake can prove here is the part that is pure sequencing: that the
// second Ensure happens, that registration precedes the console URL, that a
// failure backs off rather than spinning. What it cannot prove is that the
// resulting organisation serves anything, which is what
// internal/cloud/provision's ATLANTIS_TEST_K8S tests are for. Neither replaces
// the other, and this file is deliberately not where a "provisioning works"
// claim is made.

// ---------------------------------------------------------------- fake queue

type failedMark struct {
	org     string
	reason  string
	retryIn time.Duration
}

type auditRow struct {
	org    string
	actor  string
	email  string
	action string
}

type fakeQueue struct {
	mu sync.Mutex

	queued []store.Claimed

	claimErr  error
	extendErr error
	markErr   error
	urlErr    error

	extends     int
	provisioned []string
	failed      []failedMark
	consoleURLs map[string]string
	audits      []auditRow

	// events records the order of the writes that must not be reordered.
	events []string
}

func newFakeQueue(orgs ...store.Claimed) *fakeQueue {
	return &fakeQueue{queued: orgs, consoleURLs: map[string]string{}}
}

// ctxErr is what a real query does on a cancelled context, and the fake has to
// do it too.
//
// Without this, every write below succeeds whether or not the caller detached
// from the shutdown context — so the test asserting that a shutdown releases
// the claim passed against a version that used the cancelled context directly
// and would have written nothing at all in production. A fake that is more
// forgiving than the thing it stands in for turns a test into a formality.
func ctxErr(ctx context.Context) error { return ctx.Err() }

func (f *fakeQueue) ClaimForProvisioning(ctx context.Context, claimedBy string, _ time.Duration) (*store.Claimed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if claimedBy == "" {
		return nil, errors.New("a claimant name is required")
	}
	if len(f.queued) == 0 {
		return nil, store.ErrNothingToProvision
	}
	c := f.queued[0]
	f.queued = f.queued[1:]
	return &c, nil
}

func (f *fakeQueue) ExtendLease(_ context.Context, _, _ string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends++
	return f.extendErr
}

func (f *fakeQueue) MarkProvisioned(ctx context.Context, org string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if f.markErr != nil {
		return f.markErr
	}
	f.provisioned = append(f.provisioned, org)
	f.events = append(f.events, "mark-provisioned")
	return nil
}

func (f *fakeQueue) MarkProvisioningFailed(ctx context.Context, org, reason string, retryIn time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if retryIn.Milliseconds() <= 0 {
		// The real store refuses this, and a fake that accepts it would let a
		// busy loop pass the test that exists to catch one.
		return errors.New("a retry delay of at least a millisecond is required")
	}
	f.failed = append(f.failed, failedMark{org: org, reason: reason, retryIn: retryIn})
	f.events = append(f.events, "mark-failed")
	return nil
}

func (f *fakeQueue) SetConsoleURL(ctx context.Context, org, consoleURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if f.urlErr != nil {
		return f.urlErr
	}
	f.consoleURLs[org] = consoleURL
	f.events = append(f.events, "set-console-url")
	return nil
}

func (f *fakeQueue) LogAction(ctx context.Context, org, actor, actorEmail, action string, _ map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ctxErr(ctx) != nil {
		// The real LogAction swallows its error and logs it, so a cancelled
		// context loses the row silently. Modelled rather than smoothed over:
		// the audit assertions should fail if the caller stops detaching.
		return
	}
	f.audits = append(f.audits, auditRow{org: org, actor: actor, email: actorEmail, action: action})
}

func (f *fakeQueue) snapshot() ([]string, []failedMark, []string, []auditRow, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.provisioned...),
		append([]failedMark(nil), f.failed...),
		append([]string(nil), f.events...),
		append([]auditRow(nil), f.audits...),
		f.extends
}

// -------------------------------------------------------------- fake cluster

type fakeCluster struct {
	mu sync.Mutex

	// statuses are returned by successive Ensure calls; the last one repeats.
	statuses []provision.Status

	ensureErr error
	waitErr   error
	waitFor   time.Duration

	ensures int
	waits   int
}

func (f *fakeCluster) Ensure(_ context.Context, spec provision.Spec) (provision.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec.Org == "" {
		return provision.Status{}, errors.New("provision: Spec.Org is required")
	}
	f.ensures++
	if f.ensureErr != nil {
		return provision.Status{}, f.ensureErr
	}
	if len(f.statuses) == 0 {
		return provision.Status{}, nil
	}
	i := f.ensures - 1
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	return f.statuses[i], nil
}

func (f *fakeCluster) WaitReady(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.waits++
	wait, err := f.waitFor, f.waitErr
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

func (f *fakeCluster) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures, f.waits
}

// ------------------------------------------------------------------ fixtures

func testConfig() Config {
	return Config{
		CloudPGURL:     "postgres://cloud/x",
		ConsolePGURL:   "postgres://console/x",
		ConsoleDataKey: "a-keyset",
		ConsoleURL:     "http://localhost:3000",
		ClaimedBy:      "test-provisioner",
		PollInterval:   time.Millisecond,
		Lease:          time.Second,
		Heartbeat:      5 * time.Millisecond,
		RetryBase:      time.Second,
		RetryMax:       time.Minute,
		HealthAddr:     "127.0.0.1:0",
		Provision: provision.Config{
			ExternalHost:  "atl-dev.test",
			ServerImage:   "atlantis-server:local",
			SignerImage:   "atlantis-signer:local",
			PostgresImage: "atlantis-pg:17.11",
			MemcachedAddr: "memcached:11211",
			ReadyTimeout:  100 * time.Millisecond,
		},
	}
}

// readyStatus is what a second Ensure returns for a serving organisation.
func readyStatus() provision.Status {
	return provision.Status{
		Ready:               true,
		Endpoint:            "atl-dev.test:30090",
		PublicEndpoint:      "atl-dev.test:30090",
		HealthAddr:          "atl-dev.test:30081",
		SignerAddr:          "https://atl-dev.test:30070",
		CAPEM:               []byte("ca"),
		ConsoleCertPEM:      []byte("cert"),
		ConsoleKeyPEM:       []byte("key"),
		SignerCAPEM:         []byte("signer-ca"),
		SignerClientCertPEM: []byte("signer-cert"),
		SignerClientKeyPEM:  []byte("signer-key"),
	}
}

// unallocated is what a FIRST Ensure returns before the NodePorts exist.
func unallocated() provision.Status {
	return provision.Status{Ready: false}
}

func newTestWorker(t *testing.T, cfg Config, q Queue, c Cluster, reg RegisterFunc) *Worker {
	t.Helper()
	w, err := New(cfg, q, c, reg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

// ------------------------------------------------------------------ the loop

// The happy path, asserted on the sequence rather than the outcome.
//
// Two Ensure calls with one WaitReady between them. The second is not
// redundant: WaitReady proves the workloads are up, and the NodePorts are
// allocated separately — so a first Ensure legitimately reports Ready false
// with no addresses. A loop that called Ensure once would register an
// organisation with an empty endpoint, which validateOrgCredentials refuses
// with a message about host:port rather than about provisioning.
func TestAnOrganisationIsProvisionedRegisteredAndMarked(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}

	var registered []console.OrgRegistration
	reg := func(_ context.Context, pgURL, keyset string, r console.OrgRegistration) error {
		registered = append(registered, r)
		if pgURL != "postgres://console/x" || keyset != "a-keyset" {
			t.Errorf("RegisterOrg called with the wrong console credentials: %q %q", pgURL, keyset)
		}
		q.mu.Lock()
		q.events = append(q.events, "register")
		q.mu.Unlock()
		return nil
	}

	w := newTestWorker(t, testConfig(), q, c, reg)
	w.drain(context.Background())

	ensures, waits := c.counts()
	if ensures != 2 {
		t.Errorf("Ensure called %d times, want 2 — the addresses are read back after the wait", ensures)
	}
	if waits != 1 {
		t.Errorf("WaitReady called %d times, want 1", waits)
	}

	provisioned, failed, events, audits, _ := q.snapshot()
	if len(provisioned) != 1 || provisioned[0] != "acme" {
		t.Errorf("MarkProvisioned = %v, want [acme]", provisioned)
	}
	if len(failed) != 0 {
		t.Errorf("a successful provision recorded a failure: %+v", failed)
	}
	if len(registered) != 1 {
		t.Fatalf("RegisterOrg called %d times, want 1", len(registered))
	}
	if got := q.consoleURLs["acme"]; got != "http://localhost:3000" {
		t.Errorf("console_url = %q, want the configured console URL", got)
	}

	want := []string{"register", "set-console-url", "mark-provisioned"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("write order = %v, want %v", events, want)
	}

	if len(audits) != 1 || audits[0].action != "org.provisioned" {
		t.Fatalf("audit = %+v, want one org.provisioned row", audits)
	}
	if audits[0].actor != store.ProvisionerActor {
		t.Errorf("audit actor = %q, want %q — a blank actor reads as a logging bug",
			audits[0].actor, store.ProvisionerActor)
	}
	if audits[0].email != "" {
		t.Errorf("audit actor email = %q, want empty: no human did this", audits[0].email)
	}
}

// console_url is the commit point, so it must not be written when the console
// row was not.
//
// /authorize reads ConsoleURL only after membership, and an empty one raises
// ErrNoConsole. Written in this order, a crash between the two leaves an
// organisation that looks unprovisioned. Written the other way round, it leaves
// users routed at a console that refuses them — which is the failure that looks
// like a bug in the console.
func TestConsoleURLIsNotWrittenWhenRegistrationFails(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}
	reg := func(context.Context, string, string, console.OrgRegistration) error {
		return errors.New("console database is down")
	}

	w := newTestWorker(t, testConfig(), q, c, reg)
	w.drain(context.Background())

	provisioned, failed, _, _, _ := q.snapshot()
	if len(provisioned) != 0 {
		t.Errorf("MarkProvisioned ran despite a failed registration: %v", provisioned)
	}
	if _, ok := q.consoleURLs["acme"]; ok {
		t.Error("console_url was written for an organisation the console does not know about")
	}
	if len(failed) != 1 {
		t.Fatalf("failures = %+v, want exactly one", failed)
	}
	if !strings.Contains(failed[0].reason, "console database is down") {
		t.Errorf("the recorded reason does not carry the cause: %q", failed[0].reason)
	}
}

// PublicEndpoint is not copied through when it equals Endpoint.
//
// Ensure sets them equal in a single-network deployment, which is every
// deployment today. Storing the duplicate silently retires the COALESCE
// fallback in orgCredentials, and the two then diverge the first time somebody
// changes Endpoint alone — a failure that surfaces as a console handshake error
// against an address nobody set.
func TestAnIdenticalPublicEndpointIsLeftUnset(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}

	var got console.OrgRegistration
	reg := func(_ context.Context, _, _ string, r console.OrgRegistration) error {
		got = r
		return nil
	}
	w := newTestWorker(t, testConfig(), q, c, reg)
	w.drain(context.Background())

	if got.PublicEndpoint != "" {
		t.Errorf("PublicEndpoint = %q, want empty when it equals Endpoint (%q)",
			got.PublicEndpoint, got.Endpoint)
	}

	// And it IS carried when it genuinely differs, or this test would pass
	// against a mapping that dropped the field entirely.
	st := readyStatus()
	st.PublicEndpoint = "callers.example:30090"
	q2 := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c2 := &fakeCluster{statuses: []provision.Status{unallocated(), st}}
	w2 := newTestWorker(t, testConfig(), q2, c2, reg)
	w2.drain(context.Background())

	if got.PublicEndpoint != "callers.example:30090" {
		t.Errorf("PublicEndpoint = %q, want the differing value to be stored", got.PublicEndpoint)
	}
}

// The private keys survive the mapping as bytes.
//
// KeyPEM and SignerClientKeyPEM are []byte on both sides, unlike every other
// credential field. This asserts the values arrive intact rather than the type,
// which the compiler already has an opinion about.
func TestTheCredentialsArriveIntact(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}

	var got console.OrgRegistration
	reg := func(_ context.Context, _, _ string, r console.OrgRegistration) error {
		got = r
		return nil
	}
	newTestWorker(t, testConfig(), q, c, reg).drain(context.Background())

	for _, tc := range []struct{ name, got, want string }{
		{"Org", got.Org, "acme"},
		{"Endpoint", got.Endpoint, "atl-dev.test:30090"},
		{"HealthAddr", got.HealthAddr, "atl-dev.test:30081"},
		{"CAPEM", got.CAPEM, "ca"},
		{"CertPEM", got.CertPEM, "cert"},
		{"KeyPEM", string(got.KeyPEM), "key"},
		{"SignerAddr", got.SignerAddr, "https://atl-dev.test:30070"},
		{"SignerCAPEM", got.SignerCAPEM, "signer-ca"},
		{"SignerClientCertPEM", got.SignerClientCertPEM, "signer-cert"},
		{"SignerClientKeyPEM", string(got.SignerClientKeyPEM), "signer-key"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// An organisation that applied but is not serving is retried, not registered.
//
// Ensure reports this rather than erroring, so a loop that only checked the
// error would register an organisation with no addresses.
func TestAnOrganisationThatIsNotReadyIsRetried(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{statuses: []provision.Status{unallocated(), unallocated()}}
	reg := func(context.Context, string, string, console.OrgRegistration) error {
		t.Error("an organisation that is not serving was registered")
		return nil
	}

	newTestWorker(t, testConfig(), q, c, reg).drain(context.Background())

	provisioned, failed, _, audits, _ := q.snapshot()
	if len(provisioned) != 0 {
		t.Errorf("MarkProvisioned ran for an organisation that is not serving: %v", provisioned)
	}
	if len(failed) != 1 {
		t.Fatalf("failures = %+v, want exactly one", failed)
	}
	if failed[0].retryIn <= 0 {
		t.Errorf("retryIn = %s, want a positive backoff — zero means claimable "+
			"immediately, which is the busy loop the backoff exists to prevent", failed[0].retryIn)
	}
	if len(audits) != 1 || audits[0].action != "org.provision.failed" {
		t.Errorf("audit = %+v, want one org.provision.failed row", audits)
	}
}

// A shutdown mid-provision releases the claim rather than holding it.
//
// The marks run on a context detached from the cancelled one. Without that they
// would not land, and the organisation would stay claimed until its lease
// expired — turning a clean restart into a wait.
func TestShutdownMidProvisionReleasesTheClaim(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 4})
	c := &fakeCluster{
		statuses: []provision.Status{unallocated(), readyStatus()},
		waitFor:  time.Minute, // never completes; the context ends it
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	cfg := testConfig()
	newTestWorker(t, cfg, q, c, nil).drain(ctx)

	_, failed, _, _, _ := q.snapshot()
	if len(failed) != 1 {
		t.Fatalf("failures = %+v, want one — the claim must be released on shutdown", failed)
	}
	if !strings.Contains(failed[0].reason, "shut down") {
		t.Errorf("the reason does not say what happened: %q", failed[0].reason)
	}
	// A shutdown is not the organisation's fault, so it does not escalate the
	// backoff: attempt four would otherwise wait eight times the base.
	if failed[0].retryIn != cfg.RetryBase {
		t.Errorf("retryIn = %s, want the base delay %s — a shutdown must not "+
			"penalise the organisation it interrupted", failed[0].retryIn, cfg.RetryBase)
	}
}

// The lease is extended while a long wait is in progress.
//
// Sizing the lease above ReadyTimeout is enforced in validate, and this is the
// second answer to the same question — the one that survives somebody raising
// ReadyTimeout later without thinking about the queue.
func TestTheLeaseIsExtendedDuringALongWait(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	c := &fakeCluster{
		statuses: []provision.Status{unallocated(), readyStatus()},
		waitFor:  60 * time.Millisecond,
	}

	cfg := testConfig()
	cfg.Heartbeat = 5 * time.Millisecond
	newTestWorker(t, cfg, q, c, func(context.Context, string, string, console.OrgRegistration) error {
		return nil
	}).drain(context.Background())

	_, _, _, _, extends := q.snapshot()
	if extends == 0 {
		t.Error("the lease was never extended during a wait longer than the heartbeat")
	}
	// And the heartbeat stops when the wait does, rather than running for the
	// life of the process.
	before := extends
	time.Sleep(30 * time.Millisecond)
	_, _, _, _, after := q.snapshot()
	if after != before {
		t.Errorf("the heartbeat kept running after the wait finished: %d then %d", before, after)
	}
}

// A queue that cannot be read stops the drain rather than spinning against it.
func TestAnUnreachableQueueStopsTheDrain(t *testing.T) {
	q := newFakeQueue()
	q.claimErr = errors.New("connection refused")
	c := &fakeCluster{}

	newTestWorker(t, testConfig(), q, c, nil).drain(context.Background())

	if ensures, _ := c.counts(); ensures != 0 {
		t.Errorf("Ensure ran %d times against a queue that could not be read", ensures)
	}
}

// The whole backlog is drained, not one organisation per tick.
func TestTheBacklogIsDrainedInOnePass(t *testing.T) {
	q := newFakeQueue(
		store.Claimed{Org: "one", Attempts: 1},
		store.Claimed{Org: "two", Attempts: 1},
		store.Claimed{Org: "three", Attempts: 1},
	)
	c := &fakeCluster{statuses: []provision.Status{readyStatus()}}

	newTestWorker(t, testConfig(), q, c, func(context.Context, string, string, console.OrgRegistration) error {
		return nil
	}).drain(context.Background())

	provisioned, _, _, _, _ := q.snapshot()
	if len(provisioned) != 3 {
		t.Errorf("provisioned %v, want all three — a backlog should not take "+
			"one poll interval per organisation", provisioned)
	}
}

// A serving, registered organisation whose queue row could not be updated is
// not recorded as failed.
//
// It is working. Marking it failed would describe a healthy organisation as
// broken and schedule a retry that finds everything already in place.
func TestAStaleQueueRowIsNotRecordedAsAFailure(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	q.markErr = errors.New("write conflict")
	c := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}

	// The metric is asserted as well as the queue, because they came apart.
	// The outcome label was set after MarkProvisioned, so this path fell
	// through to the deferred counter still holding "failed" — the queue said
	// one thing, the dashboard said another, and a test that only read the
	// queue passed throughout.
	failedBefore := testutil.ToFloat64(attemptsTotal.WithLabelValues("failed"))
	okBefore := testutil.ToFloat64(attemptsTotal.WithLabelValues("provisioned"))

	newTestWorker(t, testConfig(), q, c, func(context.Context, string, string, console.OrgRegistration) error {
		return nil
	}).drain(context.Background())

	_, failed, _, _, _ := q.snapshot()
	if len(failed) != 0 {
		t.Errorf("a working organisation was recorded as failed: %+v", failed)
	}
	if q.consoleURLs["acme"] == "" {
		t.Error("the organisation was not registered, so this test proves nothing")
	}

	if got := testutil.ToFloat64(attemptsTotal.WithLabelValues("failed")); got != failedBefore {
		t.Errorf("the failed counter moved %v -> %v for an organisation that is serving",
			failedBefore, got)
	}
	if got := testutil.ToFloat64(attemptsTotal.WithLabelValues("provisioned")); got != okBefore+1 {
		t.Errorf("the provisioned counter went %v -> %v, want +1", okBefore, got)
	}
}

// Run returns when its context is cancelled, rather than on the next tick.
func TestRunStopsOnCancellation(t *testing.T) {
	q := newFakeQueue()
	c := &fakeCluster{}
	cfg := testConfig()
	cfg.PollInterval = time.Hour // if Run waited for a tick, this would hang

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- newTestWorker(t, cfg, q, c, nil).Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on a clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}
