package provisioner

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/console"
)

// markTimeout bounds the writes that record how an attempt ended.
//
// Short, and on a context detached from the one that may have just been
// cancelled: a mark that does not land leaves the row claimed until its lease
// expires, which is the difference between a restart that picks up where it
// left off and one that waits out the lease first.
const markTimeout = 10 * time.Second

// Queue is the part of the Cloud store this worker uses.
//
// An interface so the loop can be tested without a database. Every method is
// satisfied by *store.Store; nothing here is a reimplementation.
type Queue interface {
	ClaimForProvisioning(ctx context.Context, claimedBy string, lease time.Duration) (*store.Claimed, error)
	ExtendLease(ctx context.Context, org, claimedBy string, lease time.Duration) error
	MarkProvisioned(ctx context.Context, org string) error
	MarkProvisioningFailed(ctx context.Context, org, reason string, retryIn time.Duration) error
	SetConsoleURL(ctx context.Context, org, consoleURL string) error
	SetEnrollURL(ctx context.Context, org, enrollURL string) error
	LogAction(ctx context.Context, org, actor, actorEmail, action string, detail map[string]any)
	ReadyOrgs(ctx context.Context) ([]string, error)
	Requeue(ctx context.Context, org, reason string) error

	// The teardown half. Separate from ClaimForProvisioning; see
	// store.ClaimForPurge for the differing lease and retry policies.
	ClaimForPurge(ctx context.Context, claimedBy string, lease time.Duration) (*store.Claimed, error)
	MarkPurged(ctx context.Context, org string) error
	MarkPurgeFailed(ctx context.Context, org, reason string) error

	// Operator-requested console credential rotation. Cloud cannot write the
	// Secret these live in, so it marks the row and this process does the work.
	ConsoleRotationRequests(ctx context.Context) (map[string]time.Time, error)
	ClearConsoleRotationRequest(ctx context.Context, org string, seen time.Time) error
}

// Cluster is where organisations are built.
//
// Not provision.Target, which is Ensure plus Destroy and does not carry
// WaitReady; using it would need a type assertion for the wait.
// *provision.Kube satisfies this.
type Cluster interface {
	Ensure(ctx context.Context, spec provision.Spec) (provision.Status, error)
	WaitReady(ctx context.Context, org string) error
	Exists(ctx context.Context, org string) (bool, error)

	// Destroy removes an organisation's namespace and everything in it.
	Destroy(ctx context.Context, org string) error

	// RotateConsoleCredentials reissues the two certificates the console
	// presents to one organisation, keeping both certificate authorities.
	//
	// The window and the force flag are passed in rather than decided by the
	// cluster because the caller knows which of the two reasons applies — an
	// approaching expiry, or an operator who has asked. The result reports
	// whether it rotated and when the credential now expires, which is what
	// lets this process watch the fleet without a second read per organisation.
	RotateConsoleCredentials(
		ctx context.Context, org string, renewWithin time.Duration, force bool,
	) (provision.ConsoleRotation, error)
}

// RegisterFunc records a provisioned organisation with the console.
//
// A function rather than a store because that is the shape console.RegisterOrg
// has: it opens and closes its own pool from a URL and a keyset, so there is
// nothing for this package to hold open between organisations.
type RegisterFunc func(ctx context.Context, pgURL, keyset string, r console.OrgRegistration) error

// UnregisterFunc removes a destroyed organisation's registration from the
// console.
//
// The mirror of RegisterFunc, and it takes no keyset: removing rows needs
// nothing unsealed. Injected for the same reason — so the purge path can be
// tested without a console database.
type UnregisterFunc func(ctx context.Context, pgURL, org string) error

// ClusterFactory builds a connection to the cluster.
//
// A factory rather than a value because Kubernetes credentials expire, rotate,
// and — on a development cluster that is rebuilt rather than repaired — are
// replaced wholesale. A client built once at startup keeps presenting the old
// certificate authority and every call fails with "certificate signed by
// unknown authority", which names nothing an operator would connect to
// credentials. See reconnect.
type ClusterFactory func() (Cluster, error)

// Worker claims organisations and provisions them.
type Worker struct {
	cfg        Config
	q          Queue
	register   RegisterFunc
	unregister UnregisterFunc
	log        *slog.Logger

	newCluster ClusterFactory

	// mu guards cluster, which is replaced when credentials go stale.
	mu      sync.RWMutex
	cluster Cluster

	// degraded is set when the cluster cannot be reached with any credentials
	// this process can obtain. It fails readiness, so an orchestrator restarts
	// this pod rather than leaving it to burn attempts against organisations
	// that are not the thing at fault.
	degraded atomic.Bool
}

// New returns a worker. The register and unregister functions are optional and
// default to console.RegisterOrg and console.UnregisterOrg; tests supply their
// own to avoid needing a console database for a test about the loop.
//
// The cluster arrives as a factory, and New calls it once so a bad
// configuration is refused here rather than on the first claim.
func New(cfg Config, q Queue, newCluster ClusterFactory, register RegisterFunc, unregister UnregisterFunc, log *slog.Logger) (*Worker, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if q == nil {
		return nil, errors.New("provisioner: a queue is required")
	}
	if newCluster == nil {
		return nil, errors.New("provisioner: a cluster factory is required")
	}
	if register == nil {
		register = console.RegisterOrg
	}
	if unregister == nil {
		unregister = console.UnregisterOrg
	}
	if log == nil {
		log = slog.Default()
	}
	c, err := newCluster()
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("provisioner: the cluster factory returned nothing")
	}
	return &Worker{
		cfg: cfg, q: q, register: register, unregister: unregister, log: log,
		newCluster: newCluster, cluster: c,
	}, nil
}

// currentCluster returns the connection in use.
func (w *Worker) currentCluster() Cluster {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cluster
}

// Healthy reports whether this provisioner can still do its job.
//
// False once the cluster has refused this process's credentials and a rebuilt
// connection did not help. Wired into /readyz so the failure is visible to an
// orchestrator and to a person, instead of appearing as a growing pile of
// failed organisations that have nothing wrong with them.
func (w *Worker) Healthy() bool { return !w.degraded.Load() }

// reconnect rebuilds the cluster connection and reports whether it worked.
//
// Credentials are read afresh: controller-runtime's GetConfig reads the
// kubeconfig or the service account at the moment it is called, so a rotated
// authority is picked up without a restart. A failed rebuild marks this process
// degraded.
func (w *Worker) reconnect() bool {
	c, err := w.newCluster()
	if err != nil || c == nil {
		w.degraded.Store(true)
		w.log.Error("could not rebuild the cluster connection — this provisioner "+
			"cannot provision anything until its credentials work", "err", err)
		return false
	}
	w.mu.Lock()
	w.cluster = c
	w.mu.Unlock()
	w.degraded.Store(false)
	w.log.Info("rebuilt the cluster connection with fresh credentials")
	return true
}

// credentialError reports whether err means the cluster refused this process's
// credentials.
//
// Matched on text because the errors arrive from three layers that do not share
// a type: crypto/x509 for a chain that does not verify, and the API server for
// 401 and 403. A false positive costs one wasted reconnect; a false negative
// marks an organisation failed for a fault that is not its own.
func credentialError(err error) bool {
	if err == nil {
		return false
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return true
	}
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		return true
	}
	s := err.Error()
	for _, marker := range []string{
		"certificate signed by unknown authority",
		"tls: failed to verify certificate",
		"the server has asked for the client to provide credentials",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// Run drains the queue, then drains it again on every tick until ctx is done.
//
// Drain-then-tick rather than tick-then-drain, so a provisioner started against
// a backlog begins immediately instead of idling for one interval — the shape
// clients/go/jobs/runner.go uses for the same reason.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("provisioner running",
		"claimed_by", w.cfg.ClaimedBy,
		"poll_interval", w.cfg.PollInterval,
		"lease", w.cfg.Lease,
		"ready_timeout", w.cfg.Provision.ReadyTimeout)

	w.drain(ctx)

	t := time.NewTicker(w.cfg.PollInterval)
	defer t.Stop()
	r := time.NewTicker(w.cfg.ReconcileInterval)
	defer r.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("provisioner stopped")
			return nil
		case <-t.C:
			w.drain(ctx)
		case <-r.C:
			w.reconcile(ctx)
			w.drain(ctx)
			// After the drain, not before. reap destroys namespaces, and
			// running it while the drain still has claimable work would put the
			// slowest, most destructive pass in front of organisations waiting
			// to be created.
			w.reap(ctx)
		}
	}
}

// reconcile puts back in the queue any organisation the cluster has lost, and
// checks the console certificate's remaining life, this being the only loop
// that visits every ready organisation.
//
// Nothing else reads a ready row: the claim predicate covers 'pending',
// 'failed' and expired 'provisioning' only. It compares existence, not shape.
//
// Requeue rather than repair: the authority went with the namespace.
func (w *Worker) reconcile(ctx context.Context) {
	if !w.Healthy() || ctx.Err() != nil {
		return
	}
	orgs, err := w.q.ReadyOrgs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("could not list organisations to reconcile", "err", err)
		}
		return
	}

	// Read once for the pass rather than per organisation, for a column that is
	// nearly always NULL.
	//
	// A failure is not fatal to the pass. Expiry-driven rotation and the
	// existence check below still run, and the request is honoured on the next
	// interval.
	requests, err := w.q.ConsoleRotationRequests(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Error("could not read console rotation requests; "+
				"reconciling without them this pass", "err", err)
		}
		requests = nil
	}

	// The soonest console credential expiry seen this pass, for the gauge below.
	var soonest time.Time

	c := w.currentCluster()
	for _, org := range orgs {
		if ctx.Err() != nil {
			return
		}
		ok, err := c.Exists(ctx, org)
		if err != nil {
			if credentialError(err) {
				// The cluster refusing us says nothing about the organisation.
				// Requeueing here would rebuild every healthy organisation in
				// the fleet because this process cannot authenticate.
				w.log.Warn("cannot reconcile: the cluster refused this provisioner's credentials",
					"err", err)
				w.reconnect()
				return
			}
			w.log.Error("could not check whether an organisation still exists",
				"org", org, "err", err)
			continue
		}
		if ok {
			// The organisation is there, so this is the moment its credentials
			// are worth looking at. Doing it before the existence check would
			// mean rotating certificates for an organisation that has gone.
			requested, wasRequested := requests[org]
			if exp := w.rotateConsole(ctx, c, org, requested, wasRequested); !exp.IsZero() {
				if soonest.IsZero() || exp.Before(soonest) {
					soonest = exp
				}
			}
			continue
		}

		reconciledTotal.Inc()
		w.log.Warn("an organisation the queue calls ready is not in the cluster; rebuilding it",
			"org", org)
		if err := w.q.Requeue(ctx, org, "the cluster no longer has this organisation"); err != nil {
			// ErrNotFound means the row moved while we were looking, which the
			// guarded update exists to lose safely.
			if !errors.Is(err, store.ErrNotFound) {
				w.log.Error("could not requeue", "org", org, "err", err)
			}
			continue
		}
		w.audit(ctx, org, "org.requeued", map[string]any{
			"reason": "absent from the cluster",
		})
	}

	// Published only when this pass actually learned an expiry.
	//
	// The previous value stands when a pass inspected nothing — an empty fleet,
	// or a cluster that refused every call. Writing zero would read as a
	// credential expiring now and fire every threshold rule. Prometheus already
	// reports a scrape target that has stopped answering.
	if !soonest.IsZero() {
		consoleCertSecondsLeft.Set(time.Until(soonest).Seconds())
	}
}

// drain provisions organisations until the queue is empty or something breaks.
//
// It keeps going rather than taking one per tick: a backlog of ten
// organisations should not take ten poll intervals to start on the last one.
func (w *Worker) drain(ctx context.Context) {
	// A degraded provisioner tries once per tick to get its credentials back
	// and otherwise claims nothing. Claiming while it cannot reach the cluster
	// would mark healthy organisations failed at one per tick, burying the one
	// fact that matters under a queue of misleading errors.
	if !w.Healthy() {
		if !w.reconnect() {
			return
		}
	}
	for {
		if ctx.Err() != nil {
			return
		}
		// Checked every iteration, not only on entry. An organisation that
		// degrades this worker mid-drain must be the last one attempted —
		// otherwise the rest of the backlog is marked failed in the same pass,
		// each with an x509 error naming nothing that is wrong with it.
		if !w.Healthy() {
			return
		}
		claimed, err := w.q.ClaimForProvisioning(ctx, w.cfg.ClaimedBy, w.cfg.Lease)
		switch {
		case errors.Is(err, store.ErrNothingToProvision):
			return
		case err != nil:
			// Reaching the queue failed. Counted separately from a failed
			// organisation: this provisions none of them, and stops the drain
			// rather than spinning against a database that is down.
			claimFailuresTotal.Inc()
			if ctx.Err() == nil {
				w.log.Error("could not claim from the provisioning queue", "err", err)
			}
			return
		}
		w.provisionOne(ctx, *claimed)
	}
}

// provisionOne takes one claimed organisation from queued to serving.
func (w *Worker) provisionOne(ctx context.Context, c store.Claimed) {
	start := time.Now()
	outcome := "failed"
	defer func() {
		attemptsTotal.WithLabelValues(outcome).Inc()
		attemptSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}()

	log := w.log.With("org", c.Org, "attempt", c.Attempts)
	log.Info("provisioning")

	st, err := w.build(ctx, c.Org)
	if err != nil {
		w.fail(ctx, c, err)
		return
	}
	if !st.Ready {
		// Ensure reports this rather than erroring when something has not
		// finished converging. Retryable, and distinguished from a failure so
		// "slow" and "broken" do not look the same on a dashboard.
		outcome = "not_ready"
		w.fail(ctx, c, errors.New("the organisation applied but is not serving yet"))
		return
	}

	if err := w.record(ctx, c.Org, st); err != nil {
		w.fail(ctx, c, err)
		return
	}

	// Set here rather than after the mark below. The organisation is serving and
	// registered at this point, so a mark that does not land is stale
	// bookkeeping and not a failed attempt; assigning after the mark leaves the
	// deferred metric holding "failed" for an organisation that is working.
	outcome = "provisioned"

	if err := w.q.MarkProvisioned(ctx, c.Org); err != nil {
		// Not a failure mark: a retry finds everything in place and converges,
		// and a failure here would describe a working organisation as broken.
		log.Error("the organisation is serving but the queue row was not updated", "err", err)
		return
	}
	log.Info("provisioned", "endpoint", st.Endpoint, "signer", st.SignerAddr,
		"took", time.Since(start).Round(time.Second))
	w.audit(ctx, c.Org, "org.provisioned", map[string]any{
		"attempts": c.Attempts,
		"endpoint": st.Endpoint,
		"took_ms":  time.Since(start).Milliseconds(),
	})
}

// build converges the organisation and returns where it got to.
//
// Two Ensure calls with a wait between them, which is the shape
// TestK8sProvisionsAWorkingOrganisation runs: WaitReady proves the workloads
// are up, and the NodePorts are allocated separately — so a first Ensure
// legitimately returns Ready false with empty addresses rather than an error.
// The second call is what produces a Status registration can use.
func (w *Worker) build(ctx context.Context, org string) (provision.Status, error) {
	c := w.currentCluster()

	// The first call is where stale credentials surface, because it is the
	// first thing to touch the API server. Rebuilding once and retrying turns a
	// rotated authority — or a development cluster recreated underneath a
	// running provisioner — into a blip rather than every organisation failing
	// with an x509 error that names nothing an operator would look at.
	_, err := c.Ensure(ctx, provision.Spec{Org: org})
	if err != nil && credentialError(err) && ctx.Err() == nil {
		w.log.Warn("the cluster refused this provisioner's credentials; rebuilding them",
			"org", org, "err", err)
		if !w.reconnect() {
			return provision.Status{}, fmt.Errorf("apply the organisation: %w", err)
		}
		c = w.currentCluster()
		_, err = c.Ensure(ctx, provision.Spec{Org: org})
	}
	if err != nil {
		return provision.Status{}, fmt.Errorf("apply the organisation: %w", err)
	}

	stop := w.heartbeat(ctx, org)
	err = c.WaitReady(ctx, org)
	stop()
	if err != nil {
		return provision.Status{}, fmt.Errorf("wait for the organisation to serve: %w", err)
	}

	st, err := c.Ensure(ctx, provision.Spec{Org: org})
	if err != nil {
		return provision.Status{}, fmt.Errorf("read back the organisation's addresses: %w", err)
	}
	return st, nil
}

// heartbeat extends the lease while a long wait is in progress, and returns a
// function that stops it and waits for the goroutine to finish.
//
// validate already enforces a lease above ReadyTimeout; this survives a later
// change to ReadyTimeout alone.
func (w *Worker) heartbeat(ctx context.Context, org string) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer func() {
			// A panic here would take the process down mid-provision and leave
			// the organisation claimed until its lease expired.
			if r := recover(); r != nil {
				w.log.Error("lease heartbeat panicked", "org", org, "panic", r)
			}
		}()
		t := time.NewTicker(w.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := w.q.ExtendLease(hbCtx, org, w.cfg.ClaimedBy, w.cfg.Lease); err != nil {
					if hbCtx.Err() != nil {
						return
					}
					// ExtendLease is guarded on claimed_by, so a failure here
					// can mean the claim is already gone, which is the one
					// condition under which two provisioners build the same
					// organisation.
					leaseExtensionFailuresTotal.Inc()
					w.log.Warn("could not extend the provisioning lease",
						"org", org, "err", err)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// record writes the console row first, then cloud.orgs.console_url, which is
// the commit point: non-empty implies the console row exists.
//
// `cloud org register` writes them in the opposite order and is also correct,
// because it can run for an organisation Cloud has never heard of. Unifying the
// two paths regresses one of them.
func (w *Worker) record(ctx context.Context, org string, st provision.Status) error {
	reg := console.OrgRegistration{
		Org:        org,
		Endpoint:   st.Endpoint,
		HealthAddr: st.HealthAddr,
		CAPEM:      string(st.CAPEM),
		CertPEM:    string(st.ConsoleCertPEM),

		// []byte on both sides. The two private-key fields are not
		// []byte-to-string conversions, so mixing them up does not compile.
		KeyPEM: st.ConsoleKeyPEM,

		PublicEndpoint: publicEndpoint(st),

		SignerAddr:          st.SignerAddr,
		SignerCAPEM:         string(st.SignerCAPEM),
		SignerClientCertPEM: string(st.SignerClientCertPEM),
		SignerClientKeyPEM:  st.SignerClientKeyPEM,
	}

	if err := w.register(ctx, w.cfg.ConsolePGURL, w.cfg.ConsoleDataKey, reg); err != nil {
		return fmt.Errorf("register with the console: %w", err)
	}
	if err := w.q.SetConsoleURL(ctx, org, w.cfg.ConsoleURL); err != nil {
		return fmt.Errorf("point Cloud at the console: %w", err)
	}
	// Best-effort where unset: an organisation with no enrolment address
	// refuses `tide login` at the poll, which names exactly what is missing.
	if w.cfg.EnrollURL != "" {
		if err := w.q.SetEnrollURL(ctx, org, w.cfg.EnrollURL); err != nil {
			return fmt.Errorf("point Cloud at the enrolment listener: %w", err)
		}
	}
	return nil
}

// publicEndpoint reports what callers dial, when that differs from what the
// console dials.
//
// Returns "" when they are equal, which is every single-network deployment.
// atl_public_endpoint is read through a COALESCE fallback on atl_endpoint, so
// storing a duplicate retires that fallback, and the two diverge the first time
// Endpoint alone changes.
func publicEndpoint(st provision.Status) string {
	if st.PublicEndpoint == st.Endpoint {
		return ""
	}
	return st.PublicEndpoint
}

// fail records an attempt that did not finish, and schedules the next one.
func (w *Worker) fail(ctx context.Context, c store.Claimed, cause error) {
	retryIn := w.cfg.backoff(c.Attempts)
	switch {
	case ctx.Err() != nil:
		// A shutdown is not the organisation's fault, so it does not escalate
		// the backoff. The attempt is still recorded as failed because it is:
		// nothing is serving, and the row must be claimable again.
		retryIn = w.cfg.RetryBase
		cause = fmt.Errorf("the provisioner shut down mid-attempt: %w", cause)
	case credentialError(cause):
		// Nor is this. Escalating would push every queued organisation into a
		// long backoff for a platform fault, with credentials unnamed.
		retryIn = w.cfg.RetryBase
		cause = fmt.Errorf("the cluster refused this provisioner's credentials, "+
			"which is a fault in the provisioner and not in this organisation: %w", cause)
	}

	w.log.Error("provisioning did not finish",
		"org", c.Org, "attempt", c.Attempts, "retry_in", retryIn, "err", cause)

	// Detached from ctx, whose cancellation is often the cause. A mark issued
	// on a cancelled context does not land, leaving the row claimed until the
	// lease expires and turning a clean restart into a wait.
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()

	if err := w.q.MarkProvisioningFailed(mctx, c.Org, cause.Error(), retryIn); err != nil {
		w.log.Error("could not record the failure — the lease will have to expire",
			"org", c.Org, "err", err)
	}
	w.audit(mctx, c.Org, "org.provision.failed", map[string]any{
		"attempts": c.Attempts,
		"error":    cause.Error(),
		"retry_in": retryIn.String(),
	})
}

// audit writes one row to cloud.audit_log.
//
// The actor is a named constant, so a row this process wrote is distinguishable
// from one that lost its actor. No actor email, as with enrolmentActor in the
// console.
//
// LogAction returns nothing and logs its own failures: a failed audit write
// must not fail the provisioning.
func (w *Worker) audit(ctx context.Context, org, action string, detail map[string]any) {
	w.q.LogAction(ctx, org, store.ProvisionerActor, "", action, detail)
}

// rotateConsole reissues org's console credentials if they expire within
// ConsoleCertRenewWithin, or if requested is set. It returns the expiry, or the
// zero time if this pass could not read one.
//
// Failures are logged, not requeued: a rebuild mints a new authority and
// invalidates every caller certificate. requestedAt is cleared only after
// registration succeeds.
func (w *Worker) rotateConsole(
	ctx context.Context, c Cluster, org string, requestedAt time.Time, requested bool,
) time.Time {
	r, err := c.RotateConsoleCredentials(
		ctx, org, w.cfg.ConsoleCertRenewWithin, requested)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		// The expiry is carried out of every failure that got far enough to
		// read a certificate. Discarding it would freeze the fleet gauge at the
		// last healthy reading while the credential ran out.
		w.warnIfExpiringSoon(org, r.ExpiresAt)
		if credentialError(err) {
			w.log.Warn("cannot rotate console credentials: the cluster refused "+
				"this provisioner's credentials", "org", org, "err", err)
			w.reconnect()
			return r.ExpiresAt
		}
		consoleRotationFailures.Inc()
		w.log.Error("could not rotate an organisation's console credentials",
			"org", org, "requested", requested, "err", err)
		return r.ExpiresAt
	}
	if !r.Rotated {
		// Nothing was due, so no warning: reaching the window is what makes a
		// credential due.
		return r.ExpiresAt
	}

	// Re-registering is what puts the new certificate into use. The console
	// notices within orgClientRefresh because the row's updated_at moves.
	if err := w.record(ctx, org, r.Status); err != nil {
		consoleRotationFailures.Inc()
		w.log.Error("rotated an organisation's console credentials but could not "+
			"register them; the console keeps using the previous certificate "+
			"until this succeeds", "org", org, "err", err)
		// The previous certificate is the one still in use, so its expiry is
		// what describes this organisation; nothing is presenting the new one
		// until registration succeeds.
		w.warnIfExpiringSoon(org, r.PreviousExpiresAt)
		return r.PreviousExpiresAt
	}

	consoleRotationsTotal.Inc()
	w.audit(ctx, org, "org.console_credentials_rotated", map[string]any{
		"requested_by_operator": requested,
	})

	if requested {
		// Bounded by the timestamp this pass saw, so a request made while the
		// rotation was running is not thrown away with the one it satisfied.
		if err := w.q.ClearConsoleRotationRequest(ctx, org, requestedAt); err != nil {
			w.log.Error("rotated an organisation's console credentials but could "+
				"not clear the request; it will rotate again next pass",
				"org", org, "err", err)
		}
	}
	return r.ExpiresAt
}

// warnIfExpiringSoon says so in the log when a credential that is still in use
// is inside the renewal window.
//
// consoleCertSecondsLeft is the fleet minimum and cannot name the organisation.
//
// Called only from the failure paths — a rotation that errored, and one written
// to the cluster but never registered. On the not-rotated path it could not
// fire, since reaching the window is what makes a credential due.
func (w *Worker) warnIfExpiringSoon(org string, expires time.Time) {
	if expires.IsZero() {
		return
	}
	left := time.Until(expires)
	if left > w.cfg.ConsoleCertRenewWithin {
		return
	}
	w.log.Warn("an organisation's console credential is inside the renewal window "+
		"and was not rotated; when it expires this organisation cannot be opened "+
		"in a browser",
		"org", org,
		"expires", expires.Format(time.RFC3339),
		"remaining", left.Round(time.Hour).String())
}

// reap purges organisations whose retention window has elapsed. It runs on the
// reconcile tick rather than the poll tick, so the queue table is not scanned
// every ten seconds for work that occurs a few times a month.
//
// Eligibility is decided entirely by store.ClaimForPurge: state 'deleted' with
// purge_after in the past, or state 'purging' under an expired lease. This
// function does not re-check it.
func (w *Worker) reap(ctx context.Context) {
	// The same degraded check drain does. A provisioner that cannot reach the
	// cluster must not claim a purge: it would fail, release, and be reclaimed
	// on the next pass, burning through the backlog marking each one failed
	// with an error about credentials rather than about the organisation.
	if !w.Healthy() {
		if !w.reconnect() {
			return
		}
	}
	for {
		if ctx.Err() != nil || !w.Healthy() {
			return
		}
		claimed, err := w.q.ClaimForPurge(ctx, w.cfg.ClaimedBy, w.cfg.Lease)
		switch {
		case errors.Is(err, store.ErrNothingToPurge):
			return
		case err != nil:
			claimFailuresTotal.Inc()
			if ctx.Err() == nil {
				w.log.Error("could not claim from the purge queue", "err", err)
			}
			return
		}
		w.purgeOne(ctx, claimed.Org)
	}
}

// purgeOne tears down one organisation and removes its rows.
//
// Cluster first, database second. A crash between them leaves the namespace
// gone and the row reading 'purging', which the next pass reclaims; Destroy on
// an absent namespace succeeds, so the retry converges.
//
// The other order deletes the row naming the namespace to destroy.
func (w *Worker) purgeOne(ctx context.Context, org string) {
	start := time.Now()
	outcome := "failed"
	defer func() {
		purgesTotal.WithLabelValues(outcome).Inc()
	}()

	log := w.log.With("org", org)
	log.Info("purging", "note", "the retention window has passed")

	// The lease covers a claim; a teardown that outlives it would be reclaimed
	// by a second provisioner while this one is still working. Same machinery
	// the provision path uses.
	stop := w.heartbeat(ctx, org)
	defer stop()

	if err := w.currentCluster().Destroy(ctx, org); err != nil {
		// Released rather than marked terminally failed, so a customer who
		// asked to be deleted does not stay undeleted in silence.
		log.Error("could not destroy the organisation", "err", err)
		if mErr := w.q.MarkPurgeFailed(ctx, org, err.Error()); mErr != nil {
			log.Error("could not release the purge claim", "err", mErr)
		}
		return
	}

	// The console's copy, which the destroyed namespace does not take with it:
	// a sealed private key, sessions, enrolment tokens and certificate
	// fingerprints, none of which means anything once the authority is gone.
	//
	// Between Destroy and MarkPurged. Before Destroy it would strip a live
	// organisation's credentials and might then fail to destroy it; after
	// MarkPurged the row would be gone and nothing would retry.
	if err := w.unregister(ctx, w.cfg.ConsolePGURL, org); err != nil {
		// The claim is released so the next pass picks it up; Destroy is
		// idempotent against an absent namespace, so the retry converges.
		log.Error("the organisation was destroyed but the console still holds "+
			"its credentials", "err", err)
		if mErr := w.q.MarkPurgeFailed(ctx, org, err.Error()); mErr != nil {
			log.Error("could not release the purge claim", "err", mErr)
		}
		return
	}

	if err := w.q.MarkPurged(ctx, org); err != nil {
		// The namespace is gone and the rows are not. The next pass reclaims
		// the row and Destroy succeeds against an absent namespace, so this is
		// a log rather than a failure mark.
		log.Error("the organisation was destroyed but its rows remain", "err", err)
		return
	}

	outcome = "purged"
	log.Info("purged", "took", time.Since(start).Round(time.Second))

	// The audit log references the organisation by name and outlives it, so
	// "what happened to acme" still has an answer after acme stops existing.
	w.audit(ctx, org, "org.purged", map[string]any{
		"took_ms": time.Since(start).Milliseconds(),
	})
}
