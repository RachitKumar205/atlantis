package provisioner

import (
	"context"
	"errors"
	"fmt"
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

	// ready is what the queue believes is serving; requeued records what
	// reconciliation put back.
	ready    []string
	readyErr error
	requeued []string

	// events records the order of the writes that must not be reordered.
	events []string

	// The teardown side. purgeQueue is what ClaimForPurge hands out; purged and
	// purgeFailed record where each one ended up.
	purgeQueue  []store.Claimed
	purgeClaims int
	purgeErr    error
	purged      []string
	purgeFailed []failedMark

	// Console credential rotation requests. rotateRequests is what the queue
	// reports outstanding; rotateCleared records what was cleared and against
	// which observed timestamp, which is the part that must not lose a request
	// made while a rotation was running.
	rotateRequests    map[string]time.Time
	rotateRequestsErr error
	rotateCleared     []clearedRotation
	rotateClearErr    error
}

type clearedRotation struct {
	org  string
	seen time.Time
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

func (f *fakeQueue) ReadyOrgs(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if f.readyErr != nil {
		return nil, f.readyErr
	}
	return append([]string(nil), f.ready...), nil
}

func (f *fakeQueue) Requeue(ctx context.Context, org, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	// The real update is guarded on state = 'ready', so an organisation that is
	// no longer ready is a no-op reporting ErrNotFound. Modelled, because the
	// reconciler is expected to tolerate exactly that.
	for i, o := range f.ready {
		if o == org {
			f.ready = append(f.ready[:i:i], f.ready[i+1:]...)
			f.requeued = append(f.requeued, org)
			f.events = append(f.events, "requeue")
			return nil
		}
	}
	return fmt.Errorf("%s: %w", org, store.ErrNotFound)
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

	// absent names organisations the cluster no longer has; nil means every
	// organisation exists.
	absent      map[string]bool
	existsErr   error
	existsCalls int

	destroyed  []string
	destroyErr error
	// onDestroy, when set, runs inside Destroy so a test can order it against
	// the queue's writes.
	onDestroy func(org string)

	ensures int
	waits   int

	// Console credential rotation. rotateDue names the organisations whose
	// credentials the cluster considers near expiry; rotateCalls records every
	// request so a test can assert the window and the force flag the worker
	// passed, not merely the outcome.
	rotateDue    map[string]bool
	rotateCalls  []rotateCall
	rotateErr    error
	rotateStatus provision.Status
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

func (f *fakeCluster) Exists(ctx context.Context, org string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return false, err
	}
	if f.existsErr != nil {
		return false, f.existsErr
	}
	f.existsCalls++
	if f.absent == nil {
		return true, nil
	}
	return !f.absent[org], nil
}

func (f *fakeCluster) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ensures, f.waits
}

// ------------------------------------------------------------------ fixtures

func testConfig() Config {
	return Config{
		CloudPGURL:        "postgres://cloud/x",
		ConsolePGURL:      "postgres://console/x",
		ConsoleDataKey:    "a-keyset",
		ConsoleURL:        "http://localhost:3000",
		ClaimedBy:         "test-provisioner",
		PollInterval:      time.Millisecond,
		ReconcileInterval: time.Hour,
		// Whatever a test asserts about rotation comes from fakeCluster's
		// rotateDue, not from this. It is here because validate refuses a
		// non-positive window — a rotation that only fires once the certificate
		// has expired is the outage it exists to prevent.
		ConsoleCertRenewWithin: 10 * 24 * time.Hour,
		Lease:                  time.Second,
		Heartbeat:              5 * time.Millisecond,
		RetryBase:              time.Second,
		RetryMax:               time.Minute,
		HealthAddr:             "127.0.0.1:0",
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

// fakeFactory hands out clusters in order; the last one repeats.
//
// It exists so a test can say "the connection this process holds stops working,
// and rebuilding it produces a working one" — which is what a rotated authority
// looks like from inside the provisioner.
type fakeFactory struct {
	mu       sync.Mutex
	clusters []Cluster
	err      error // when set, rebuilding fails
	calls    int
}

func (f *fakeFactory) next() (Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil && f.calls > 1 {
		return nil, f.err
	}
	i := f.calls - 1
	if i >= len(f.clusters) {
		i = len(f.clusters) - 1
	}
	return f.clusters[i], nil
}

func (f *fakeFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestWorker(t *testing.T, cfg Config, q Queue, c Cluster, reg RegisterFunc) *Worker {
	t.Helper()
	return newTestWorkerF(t, cfg, q, &fakeFactory{clusters: []Cluster{c}}, reg)
}

func newTestWorkerF(t *testing.T, cfg Config, q Queue, f *fakeFactory, reg RegisterFunc) *Worker {
	t.Helper()
	w, err := New(cfg, q, f.next, reg, nil)
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

// ------------------------------------------------------------- reconciliation

// An organisation the queue calls ready but the cluster has lost is requeued.
//
// The case that produced this: the local cluster was destroyed and rebuilt, and
// three organisations stayed 'ready' with no namespace. Nothing looks at a ready
// row — the claim predicate covers pending, failed and expired-provisioning and
// deliberately not ready — so recovering them meant hand-written SQL.
func TestAnAbsentOrganisationIsRequeued(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"gone", "here"}
	c := &fakeCluster{absent: map[string]bool{"gone": true}}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued, stillReady := append([]string(nil), q.requeued...), append([]string(nil), q.ready...)
	q.mu.Unlock()

	if len(requeued) != 1 || requeued[0] != "gone" {
		t.Errorf("requeued = %v, want [gone]", requeued)
	}
	if len(stillReady) != 1 || stillReady[0] != "here" {
		t.Errorf("still ready = %v, want [here] — an organisation that exists must "+
			"not be rebuilt, which would mint a new authority and invalidate every "+
			"caller certificate under the old one", stillReady)
	}

	_, _, _, audits, _ := q.snapshot()
	if len(audits) != 1 || audits[0].action != "org.requeued" || audits[0].org != "gone" {
		t.Errorf("audit = %+v, want one org.requeued row for gone", audits)
	}
}

// A cluster that refuses our credentials must not requeue the whole fleet.
//
// The dangerous shape: Exists fails for every organisation, and a reconciler
// that treated an error as absence would rebuild every customer at once —
// destroying every certificate authority in the process, because this process
// could not authenticate.
func TestACredentialFailureDoesNotRequeueEverything(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"one", "two", "three"}
	c := &fakeCluster{existsErr: staleCredsErr}
	f := &fakeFactory{clusters: []Cluster{c}, err: errors.New("no kubeconfig")}

	w := newTestWorkerF(t, testConfig(), q, f, nil)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued := append([]string(nil), q.requeued...)
	q.mu.Unlock()
	if len(requeued) != 0 {
		t.Errorf("requeued %v while the cluster was refusing our credentials — "+
			"every one of those organisations was fine", requeued)
	}
	if w.Healthy() {
		t.Error("the worker did not degrade after the cluster refused it during reconciliation")
	}
}

// An ordinary lookup failure skips that organisation and carries on.
func TestAnUncheckableOrganisationIsSkippedNotRequeued(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{existsErr: errors.New("etcdserver: request timed out")}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued := append([]string(nil), q.requeued...)
	q.mu.Unlock()
	if len(requeued) != 0 {
		t.Errorf("requeued %v on a lookup error — 'I could not tell' is not 'it is gone'", requeued)
	}
	if !w.Healthy() {
		t.Error("a transient lookup error degraded the worker; only credential failures should")
	}
}

// Reconciliation does nothing while degraded.
func TestReconcileDoesNothingWhileDegraded(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{absent: map[string]bool{"one": true}}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.degraded.Store(true)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued := append([]string(nil), q.requeued...)
	q.mu.Unlock()
	if len(requeued) != 0 {
		t.Errorf("a degraded worker requeued %v; it cannot know anything about the cluster", requeued)
	}
}

// ---------------------------------------------------- stale cluster credentials

// staleCredsErr is the error the walkthrough actually produced, verbatim.
//
// It arrived when the local cluster was destroyed and rebuilt underneath a
// running provisioner: the new cluster has a new certificate authority, and the
// client this process built at startup still presents the old one. Nothing in
// the message mentions credentials, so it reads as a broken organisation.
var staleCredsErr = errors.New(`Patch "https://atl-dev.test:6443/api/v1/namespaces/org-acme?fieldManager=atlantis-provisioner&force=true": tls: failed to verify certificate: x509: certificate signed by unknown authority`)

func TestCredentialErrorsAreRecognised(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the walkthrough's own error", staleCredsErr, true},
		{"unknown authority", errors.New("x509: certificate signed by unknown authority"), true},
		{"asked for credentials", errors.New("the server has asked for the client to provide credentials"), true},
		{"wrapped", fmt.Errorf("apply: %w", staleCredsErr), true},
		{"nil", nil, false},
		{"an ordinary failure", errors.New("no such image atlantis-pg:17.11"), false},
		{"a timeout", context.DeadlineExceeded, false},
	} {
		if got := credentialError(tc.err); got != tc.want {
			t.Errorf("%s: credentialError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Stale credentials are rebuilt and the organisation provisions anyway.
//
// The failure this prevents: every organisation in the queue marked failed, one
// per tick, with an x509 error that names none of them and no way to recover
// short of somebody noticing and restarting the process.
func TestStaleCredentialsAreRebuiltAndTheOrganisationSucceeds(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	broken := &fakeCluster{ensureErr: staleCredsErr}
	working := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}
	f := &fakeFactory{clusters: []Cluster{broken, working}}

	w := newTestWorkerF(t, testConfig(), q, f, func(context.Context, string, string, console.OrgRegistration) error {
		return nil
	})
	w.drain(context.Background())

	if f.count() != 2 {
		t.Errorf("the cluster connection was rebuilt %d times, want 2 (one at New, one on the refusal)", f.count())
	}
	provisioned, failed, _, _, _ := q.snapshot()
	if len(provisioned) != 1 {
		t.Errorf("provisioned = %v, want [acme] — a rebuilt connection should carry the attempt through", provisioned)
	}
	if len(failed) != 0 {
		t.Errorf("the organisation was marked failed despite a working retry: %+v", failed)
	}
	if !w.Healthy() {
		t.Error("the worker stayed degraded after a successful rebuild")
	}
}

// When the rebuild also fails, the worker degrades rather than blaming
// organisations.
func TestCredentialsThatCannotBeRebuiltDegradeTheWorker(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 5})
	broken := &fakeCluster{ensureErr: staleCredsErr}
	f := &fakeFactory{clusters: []Cluster{broken}, err: errors.New("no kubeconfig")}

	cfg := testConfig()
	w := newTestWorkerF(t, cfg, q, f, nil)
	w.drain(context.Background())

	if w.Healthy() {
		t.Error("the worker reports healthy after the cluster refused it and the rebuild failed")
	}

	_, failed, _, _, _ := q.snapshot()
	if len(failed) != 1 {
		t.Fatalf("failures = %+v, want one", failed)
	}
	// Attempt five would otherwise back off sixteen times the base. The
	// organisation did nothing wrong and must not be punished for a fault in
	// this process.
	if failed[0].retryIn != cfg.RetryBase {
		t.Errorf("retryIn = %s, want the base %s — a credential fault is not the "+
			"organisation's fault and must not escalate its backoff",
			failed[0].retryIn, cfg.RetryBase)
	}
	if !strings.Contains(failed[0].reason, "credentials") {
		t.Errorf("the recorded reason does not name credentials, so whoever reads "+
			"the row will look at the organisation: %q", failed[0].reason)
	}
}

// A degraded worker claims nothing until its credentials work again.
func TestADegradedWorkerStopsClaiming(t *testing.T) {
	q := newFakeQueue(
		store.Claimed{Org: "one", Attempts: 1},
		store.Claimed{Org: "two", Attempts: 1},
		store.Claimed{Org: "three", Attempts: 1},
	)
	broken := &fakeCluster{ensureErr: staleCredsErr}
	f := &fakeFactory{clusters: []Cluster{broken}, err: errors.New("no kubeconfig")}

	w := newTestWorkerF(t, testConfig(), q, f, nil)
	w.drain(context.Background()) // fails "one", degrades
	w.drain(context.Background()) // must not touch "two"
	w.drain(context.Background())

	_, failed, _, _, _ := q.snapshot()
	if len(failed) != 1 {
		t.Errorf("marked %d organisations failed, want 1 — a provisioner that cannot "+
			"reach the cluster should stop, not work through the queue burying the "+
			"one fact that matters", len(failed))
	}
}

// And it recovers on its own once the credentials work.
func TestADegradedWorkerRecovers(t *testing.T) {
	q := newFakeQueue(store.Claimed{Org: "acme", Attempts: 1})
	broken := &fakeCluster{ensureErr: staleCredsErr}
	working := &fakeCluster{statuses: []provision.Status{unallocated(), readyStatus()}}
	f := &fakeFactory{clusters: []Cluster{broken, broken}, err: errors.New("no kubeconfig")}

	w := newTestWorkerF(t, testConfig(), q, f, func(context.Context, string, string, console.OrgRegistration) error {
		return nil
	})
	w.drain(context.Background())
	if w.Healthy() {
		t.Fatal("the worker did not degrade, so this test proves nothing")
	}

	// The cluster comes back, and the organisation's backoff elapses so it is
	// claimable again — which the real predicate does on next_attempt_after and
	// this fake has no clock for.
	f.mu.Lock()
	f.err = nil
	f.clusters = []Cluster{working}
	f.calls = 0
	f.mu.Unlock()
	q.mu.Lock()
	q.queued = append(q.queued, store.Claimed{Org: "acme", Attempts: 2})
	q.mu.Unlock()

	w.drain(context.Background())

	if !w.Healthy() {
		t.Error("the worker never recovered, so it needs a restart it should not need")
	}
	provisioned, _, _, _, _ := q.snapshot()
	if len(provisioned) != 1 {
		t.Errorf("provisioned = %v after recovery, want [acme]", provisioned)
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

// The teardown half of the fakes.
//
// Every method honours ctxErr for the reason the comment on it gives: a fake
// more forgiving than a real query turns a shutdown test into a formality. The
// purge path is where that matters most — a reaper that kept destroying after
// its context was cancelled would be tearing down namespaces during a shutdown.

func (f *fakeQueue) ClaimForPurge(ctx context.Context, claimedBy string, _ time.Duration) (*store.Claimed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	f.purgeClaims++
	if f.purgeErr != nil {
		return nil, f.purgeErr
	}
	if len(f.purgeQueue) == 0 {
		return nil, store.ErrNothingToPurge
	}
	c := f.purgeQueue[0]
	f.purgeQueue = f.purgeQueue[1:]
	return &c, nil
}

func (f *fakeQueue) MarkPurged(ctx context.Context, org string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	f.purged = append(f.purged, org)
	f.events = append(f.events, "purged:"+org)
	return nil
}

func (f *fakeQueue) MarkPurgeFailed(ctx context.Context, org, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	f.purgeFailed = append(f.purgeFailed, failedMark{org: org, reason: reason})
	return nil
}

func (f *fakeQueue) ConsoleRotationRequests(ctx context.Context) (map[string]time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if f.rotateRequestsErr != nil {
		return nil, f.rotateRequestsErr
	}
	out := map[string]time.Time{}
	for k, v := range f.rotateRequests {
		out[k] = v
	}
	return out, nil
}

func (f *fakeQueue) ClearConsoleRotationRequest(ctx context.Context, org string, seen time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if f.rotateClearErr != nil {
		return f.rotateClearErr
	}
	f.rotateCleared = append(f.rotateCleared, clearedRotation{org: org, seen: seen})
	return nil
}

// rotateCall records one RotateConsoleCredentials call, so a test can assert
// what the worker asked for rather than only what it did with the answer.
type rotateCall struct {
	org         string
	renewWithin time.Duration
	force       bool
}

func (c *fakeCluster) RotateConsoleCredentials(
	ctx context.Context, org string, renewWithin time.Duration, force bool,
) (provision.Status, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctxErr(ctx); err != nil {
		return provision.Status{}, false, err
	}
	c.rotateCalls = append(c.rotateCalls, rotateCall{
		org: org, renewWithin: renewWithin, force: force,
	})
	if c.rotateErr != nil {
		return provision.Status{}, false, c.rotateErr
	}
	// Rotates when asked to, or when the test has said this organisation is
	// due. A fake that always rotated would make "only rotates what is due"
	// untestable.
	if force || c.rotateDue[org] {
		return c.rotateStatus, true, nil
	}
	return provision.Status{}, false, nil
}

func (c *fakeCluster) Destroy(ctx context.Context, org string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.destroyErr != nil {
		return c.destroyErr
	}
	c.destroyed = append(c.destroyed, org)
	if c.onDestroy != nil {
		c.onDestroy(org)
	}
	return nil
}

// ------------------------------------------------------------------- reaping

func TestReapDestroysTheNamespaceThenTheRows(t *testing.T) {
	q := newFakeQueue()
	q.purgeQueue = []store.Claimed{{Org: "gone"}}
	c := &fakeCluster{}
	w := newTestWorker(t, testConfig(), q, c, nil)

	w.reap(context.Background())

	if len(c.destroyed) != 1 || c.destroyed[0] != "gone" {
		t.Fatalf("cluster destroyed %v, want [gone]", c.destroyed)
	}
	if len(q.purged) != 1 || q.purged[0] != "gone" {
		t.Fatalf("queue purged %v, want [gone]", q.purged)
	}
}

// The order is the design, so it is asserted rather than assumed.
//
// Cluster first, rows second. The other order deletes the row that says which
// namespace to destroy, stranding a live namespace holding a customer's data
// with nothing left pointing at it — and no later pass can find it, because
// finding it is what the row was for.
func TestReapDestroysBeforeItForgets(t *testing.T) {
	q := newFakeQueue()
	q.purgeQueue = []store.Claimed{{Org: "ordered"}}
	c := &fakeCluster{}
	// Recorded through the shared events slice so the two writes are ordered
	// against each other rather than each against itself.
	c.onDestroy = func(org string) {
		q.mu.Lock()
		q.events = append(q.events, "destroyed:"+org)
		q.mu.Unlock()
	}
	w := newTestWorker(t, testConfig(), q, c, nil)

	w.reap(context.Background())

	want := []string{"destroyed:ordered", "purged:ordered"}
	if len(q.events) != len(want) {
		t.Fatalf("events %v, want %v", q.events, want)
	}
	for i := range want {
		if q.events[i] != want[i] {
			t.Fatalf("events %v, want %v — the rows were removed before the "+
				"namespace, which strands it", q.events, want)
		}
	}
}

// A failed teardown must be retried, not buried.
//
// There is deliberately no backoff and no attempt cap on this path: an
// organisation whose owner asked for deletion and quietly was not deleted is
// the worst outcome the feature has. So a failure releases the claim and the
// next pass takes it again.
func TestAFailedTeardownReleasesTheClaimForAnotherTry(t *testing.T) {
	q := newFakeQueue()
	q.purgeQueue = []store.Claimed{{Org: "stubborn"}}
	c := &fakeCluster{destroyErr: errors.New("the API server said no")}
	w := newTestWorker(t, testConfig(), q, c, nil)

	w.reap(context.Background())

	if len(q.purged) != 0 {
		t.Fatalf("the rows were removed despite the teardown failing: %v", q.purged)
	}
	if len(q.purgeFailed) != 1 || q.purgeFailed[0].org != "stubborn" {
		t.Fatalf("purgeFailed %v, want one entry for stubborn", q.purgeFailed)
	}
	if q.purgeFailed[0].reason == "" {
		t.Error("the failure was released with no reason recorded, so an operator " +
			"has nothing to read")
	}
}

// A reaper that cannot reach the cluster must claim nothing.
//
// Claiming while degraded would walk the whole backlog marking each
// organisation failed with an error about credentials rather than about the
// organisation — the same reason drain checks this.
func TestADegradedWorkerReapsNothing(t *testing.T) {
	q := newFakeQueue()
	q.purgeQueue = []store.Claimed{{Org: "untouched"}}
	// A factory with nothing left to hand out: reconnect cannot succeed.
	f := &fakeFactory{clusters: []Cluster{&fakeCluster{}}, err: errors.New("no credentials")}
	w := newTestWorkerF(t, testConfig(), q, f, nil)
	w.degraded.Store(true)

	w.reap(context.Background())

	if q.purgeClaims != 0 {
		t.Errorf("a degraded worker claimed %d organisations for destruction", q.purgeClaims)
	}
}

// Shutdown must stop the reaper where it is.
//
// Nothing here is more important than not destroying a namespace on the way
// out. The fake honours ctxErr, so this fails if reap keeps going.
func TestReapStopsOnceTheContextIsCancelled(t *testing.T) {
	q := newFakeQueue()
	q.purgeQueue = []store.Claimed{{Org: "a"}, {Org: "b"}, {Org: "c"}}
	c := &fakeCluster{}
	w := newTestWorker(t, testConfig(), q, c, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.reap(ctx)

	if len(c.destroyed) != 0 {
		t.Errorf("a cancelled reaper destroyed %v", c.destroyed)
	}
}

// ------------------------------------------- console credential rotation

// Rotation happens on the reconcile pass, for organisations that are due.
//
// # Why this is driven from reconcile at all
//
// It is the only loop that visits every ready organisation on a schedule. A
// certificate running down is not drift — nothing changed it — but it needs the
// same fleet-wide sweep, and a second loop doing the same walk would double the
// per-tenant cost of the one loop whose cost already grows with the customer
// count.
func TestReconcileRotatesCredentialsThatAreDue(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"due", "fine"}
	c := &fakeCluster{rotateDue: map[string]bool{"due": true}}

	var registered []string
	w := newTestWorker(t, testConfig(), q, c, func(_ context.Context, _, _ string, r console.OrgRegistration) error {
		registered = append(registered, r.Org)
		return nil
	})
	w.reconcile(context.Background())

	c.mu.Lock()
	calls := append([]rotateCall(nil), c.rotateCalls...)
	c.mu.Unlock()

	// Both are asked; only one is due. Asking about every organisation is the
	// design — the cluster holds the expiry, so it is the only thing that can
	// answer — and a test that only checked the rotated one would not notice a
	// worker that had stopped asking about the rest.
	if len(calls) != 2 {
		t.Fatalf("asked the cluster about %d organisations, want 2: %+v", len(calls), calls)
	}
	if len(registered) != 1 || registered[0] != "due" {
		t.Errorf("registered %v, want only the organisation that rotated", registered)
	}
}

// The renewal window reaches the cluster, and is not quietly zero.
//
// A window of zero means "rotate only what has already expired", which is the
// outage the whole mechanism exists to avoid — and it is what a worker that
// forgot to pass the configured value would send.
func TestReconcilePassesTheConfiguredRenewalWindow(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{}

	cfg := testConfig()
	cfg.ConsoleCertRenewWithin = 72 * time.Hour
	w := newTestWorker(t, cfg, q, c, nil)
	w.reconcile(context.Background())

	c.mu.Lock()
	calls := append([]rotateCall(nil), c.rotateCalls...)
	c.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("got %d rotate calls, want 1", len(calls))
	}
	if calls[0].renewWithin != 72*time.Hour {
		t.Errorf("the worker asked with a window of %v, want 72h — the configured "+
			"value is not reaching the cluster", calls[0].renewWithin)
	}
	if calls[0].force {
		t.Error("an organisation nobody asked about was forced")
	}
}

// An operator's request forces a rotation that is not otherwise due, and the
// request is cleared afterwards.
func TestReconcileHonoursAnOperatorsRotationRequest(t *testing.T) {
	asked := time.Now().Add(-time.Minute)
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rotateRequests = map[string]time.Time{"asked": asked}
	// Deliberately not due: the request is the only reason to rotate.
	c := &fakeCluster{}

	var registered []string
	w := newTestWorker(t, testConfig(), q, c, func(_ context.Context, _, _ string, r console.OrgRegistration) error {
		registered = append(registered, r.Org)
		return nil
	})
	w.reconcile(context.Background())

	c.mu.Lock()
	calls := append([]rotateCall(nil), c.rotateCalls...)
	c.mu.Unlock()
	if len(calls) != 1 || !calls[0].force {
		t.Fatalf("the request did not force a rotation: %+v", calls)
	}
	if len(registered) != 1 {
		t.Errorf("a forced rotation registered %v, want the organisation", registered)
	}

	q.mu.Lock()
	cleared := append([]clearedRotation(nil), q.rotateCleared...)
	q.mu.Unlock()
	if len(cleared) != 1 || cleared[0].org != "asked" {
		t.Fatalf("the request was not cleared: %+v", cleared)
	}
	// Cleared against the timestamp this pass saw, which is what stops a
	// request made while the rotation was running being thrown away with the
	// one it satisfied.
	if !cleared[0].seen.Equal(asked) {
		t.Errorf("cleared against %s, want the observed request time %s — a newer "+
			"request would be discarded", cleared[0].seen, asked)
	}
}

// A rotation that could not be registered does not clear the request.
//
// The operator asked for the console to be using a different certificate, and
// that is only true once the console has been told. Clearing here would report
// the request satisfied while the console kept presenting the old credential —
// which still works, so nothing would look wrong.
func TestAnUnregisteredRotationLeavesTheRequestOutstanding(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"asked"}
	q.rotateRequests = map[string]time.Time{"asked": time.Now()}
	c := &fakeCluster{}

	w := newTestWorker(t, testConfig(), q, c, func(context.Context, string, string, console.OrgRegistration) error {
		return errors.New("console database is down")
	})
	w.reconcile(context.Background())

	q.mu.Lock()
	cleared := append([]clearedRotation(nil), q.rotateCleared...)
	q.mu.Unlock()
	if len(cleared) != 0 {
		t.Error("the request was cleared even though the console was never told; " +
			"the operator would believe the credential had been replaced")
	}
}

// A rotation that fails does not take the organisation out of service.
//
// Nothing about it is wrong: its pods are running and its callers are
// authenticating. Requeueing would rebuild it, and rebuilding mints a new
// authority — turning a failed credential refresh into the mass invalidation
// this mechanism exists to avoid.
func TestAFailedRotationDoesNotRequeueTheOrganisation(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{rotateErr: errors.New("the secret could not be written")}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued := append([]string(nil), q.requeued...)
	q.mu.Unlock()
	if len(requeued) != 0 {
		t.Errorf("a failed rotation requeued %v, which would mint a new authority "+
			"and invalidate every caller certificate", requeued)
	}
}

// The existence check still runs when the rotation request query fails.
//
// Reconcile has two jobs and only one of them depends on that query. Abandoning
// the pass would stop the fleet being checked for missing organisations because
// of an unrelated failure.
func TestReconcileStillChecksExistenceWhenRequestsCannotBeRead(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"gone"}
	q.rotateRequestsErr = errors.New("cloud database is down")
	c := &fakeCluster{absent: map[string]bool{"gone": true}}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	q.mu.Lock()
	requeued := append([]string(nil), q.requeued...)
	q.mu.Unlock()
	if len(requeued) != 1 || requeued[0] != "gone" {
		t.Errorf("requeued %v; an unreadable rotation-request query stopped the "+
			"existence check, which is the half that does not depend on it", requeued)
	}
}

// An organisation that is gone is not rotated.
//
// Rotation writes a Secret into the organisation's namespace. Doing that for an
// organisation the cluster no longer has would either fail noisily every pass or
// recreate part of a namespace that is being torn down.
func TestAnAbsentOrganisationIsNotRotated(t *testing.T) {
	q := newFakeQueue()
	q.ready = []string{"gone"}
	q.rotateRequests = map[string]time.Time{"gone": time.Now()}
	c := &fakeCluster{absent: map[string]bool{"gone": true}}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	c.mu.Lock()
	calls := append([]rotateCall(nil), c.rotateCalls...)
	c.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("tried to rotate credentials for an organisation that is not in "+
			"the cluster: %+v", calls)
	}
}

// The failure metric actually moves.
//
// It is the only warning that arrives before the outage rather than with it. A
// rotation that keeps failing changes nothing anyone can see — the organisation
// stays ready, its pods stay up, its callers keep working — right until the
// credential expires and every browser loses that organisation at once. So a
// counter that never increments would be worse than no counter, because the
// alerting rule built on it would be permanently green.
func TestAFailedRotationIsCounted(t *testing.T) {
	before := testutil.ToFloat64(consoleRotationFailures)

	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{rotateErr: errors.New("the secret could not be written")}

	w := newTestWorker(t, testConfig(), q, c, nil)
	w.reconcile(context.Background())

	if got := testutil.ToFloat64(consoleRotationFailures); got != before+1 {
		t.Errorf("the failure counter is %v, want %v — a rotation that keeps failing "+
			"would be silent until the credential expired", got, before+1)
	}
}

// A rotation that happened is counted, and one that was not due is not.
//
// The second half is the interesting one. Counting every pass would make the
// metric useless for spotting a rotation loop, which is the failure this
// counter is meant to make visible from its rate.
func TestOnlyRealRotationsAreCounted(t *testing.T) {
	before := testutil.ToFloat64(consoleRotationsTotal)

	q := newFakeQueue()
	q.ready = []string{"due", "fine"}
	c := &fakeCluster{rotateDue: map[string]bool{"due": true}}

	w := newTestWorker(t, testConfig(), q, c,
		func(context.Context, string, string, console.OrgRegistration) error { return nil })
	w.reconcile(context.Background())

	if got := testutil.ToFloat64(consoleRotationsTotal); got != before+1 {
		t.Errorf("the rotation counter is %v, want %v — two organisations were "+
			"visited and one was due", got, before+1)
	}
}

// A rotation that could not be registered counts as a failure, not a success.
//
// The certificate exists in the cluster and the console has never been told
// about it, so it is still presenting the old one. Counting that as a rotation
// would report the fleet as healthy while credentials went unreplaced.
func TestARotationThatWasNeverRegisteredCountsAsAFailure(t *testing.T) {
	okBefore := testutil.ToFloat64(consoleRotationsTotal)
	failBefore := testutil.ToFloat64(consoleRotationFailures)

	q := newFakeQueue()
	q.ready = []string{"one"}
	c := &fakeCluster{rotateDue: map[string]bool{"one": true}}

	w := newTestWorker(t, testConfig(), q, c,
		func(context.Context, string, string, console.OrgRegistration) error {
			return errors.New("console database is down")
		})
	w.reconcile(context.Background())

	if got := testutil.ToFloat64(consoleRotationsTotal); got != okBefore {
		t.Errorf("the rotation counter moved to %v; the console was never told, so "+
			"nothing was rotated as far as it is concerned", got)
	}
	if got := testutil.ToFloat64(consoleRotationFailures); got != failBefore+1 {
		t.Errorf("the failure counter is %v, want %v", got, failBefore+1)
	}
}
