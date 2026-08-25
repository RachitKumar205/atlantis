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
	LogAction(ctx context.Context, org, actor, actorEmail, action string, detail map[string]any)
	ReadyOrgs(ctx context.Context) ([]string, error)
	Requeue(ctx context.Context, org, reason string) error

	// The teardown half. Separate from ClaimForProvisioning for the reason
	// store.ClaimForPurge records: one predicate covering both would be the one
	// query nobody can afford to misread.
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
// Deliberately not provision.Target, which is Ensure plus Destroy and does not
// carry WaitReady. Programming to that interface would mean a type assertion in
// the one place that must not be clever, so this names what is actually needed
// instead. *provision.Kube satisfies it.
type Cluster interface {
	Ensure(ctx context.Context, spec provision.Spec) (provision.Status, error)
	WaitReady(ctx context.Context, org string) error
	Exists(ctx context.Context, org string) (bool, error)

	// Destroy removes an organisation's namespace and everything in it.
	//
	// Added when deletion was wired up. Until then this method existed on
	// *provision.Kube and nothing outside a test had ever called it — the
	// destructive half of provisioning, written and unreachable.
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
// Credentials are read afresh, which is the whole point: controller-runtime's
// GetConfig reads the kubeconfig or the service account at the moment it is
// called, so a rotated authority is picked up without a restart. If the rebuild
// fails, this process is degraded rather than merely unlucky.
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

// credentialError reports whether err means the cluster refused who we are.
//
// Matched on text because the errors arrive from three layers that do not share
// a type: crypto/x509 for a chain that does not verify, and the API server for
// 401 and 403. A false positive costs one wasted reconnect; a false negative
// costs an organisation marked failed for a fault that is not its own, which is
// the more expensive way to be wrong.
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

// reconcile puts back in the queue any organisation the cluster has lost.
//
// # Why this is needed at all
//
// Nothing else looks at a ready row. The claim predicate covers pending,
// failed and expired-provisioning, and deliberately not ready — so an
// organisation whose namespace is deleted, or whose cluster is rebuilt
// underneath it, stays ready for ever while serving nothing. That was found by
// running the local walkthrough: three organisations survived a cluster rebuild
// as ready rows with no namespace, and recovering them meant hand-written SQL.
//
// # What it deliberately does not do
//
// It compares existence, not shape, with one exception. A Deployment scaled to
// zero by hand, a Secret edited, a NetworkPolicy removed — none of that is
// noticed. Detecting drift inside a namespace means diffing every object this
// package applies against what is there, and a partial version of that is worse
// than none: it would report an organisation as reconciled while leaving whole
// classes of drift unchecked. Absence is the case that matters and the case that
// can be answered honestly.
//
// The exception is the console's certificate, which is checked for how much life
// it has left. That is not drift — nothing changed it — it is a clock running
// down, and there is no other loop that visits every ready organisation on a
// schedule. Keeping it here means the fleet's credentials are renewed by the
// same pass that already proves each organisation is still there.
//
// # Why requeue rather than repair in place
//
// Rebuilding mints a new certificate authority, because the old one lived in a
// Secret that went with the namespace. Every caller certificate issued under it
// stops working. That is not a choice this makes — the authority is already
// gone — but it is why a requeued organisation is a real event worth an audit
// row rather than silent self-healing.
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

	// Read once for the whole pass rather than per organisation. Almost every
	// row has no request, and the alternative is a query per tenant per
	// interval for a column that is nearly always NULL.
	//
	// A failure here is not fatal to the pass: the expiry-driven half of
	// rotation and the existence check below both still work, and an operator's
	// request is honoured on the next interval instead. Skipping the whole
	// reconcile because one query failed would stop the fleet being checked at
	// all.
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
	// Leaving the previous value standing is deliberate. A pass that inspected
	// nothing — an empty fleet, or a cluster that refused every call — knows
	// nothing new, and writing zero there would read as "a credential expires
	// now" and page somebody about a fault that is somewhere else entirely.
	// Staleness is the honest failure here, and Prometheus already reports a
	// scrape target that has stopped answering.
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
			// Reaching the queue at all failed. Counted separately from a
			// failed organisation because this provisions nobody, and stops
			// the drain rather than spinning against a database that is down.
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

	// Set here rather than after the mark below, and the difference is not
	// cosmetic: the organisation is serving and registered at this point, so a
	// mark that does not land is stale bookkeeping and not a failed attempt.
	//
	// With the assignment after the mark, that path fell through to the
	// deferred metric still holding "failed" — so the counter contradicted both
	// the comment below and the queue, and a dashboard would have shown a
	// failure for an organisation that was working.
	outcome = "provisioned"

	if err := w.q.MarkProvisioned(ctx, c.Org); err != nil {
		// Deliberately not a failure mark — a retry finds everything in place
		// and converges, whereas recording a failure here would describe a
		// working organisation as broken.
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
// Sizing the lease above ReadyTimeout is already enforced in validate, so this
// is the second of two answers to the same question rather than the only one.
// It is the one that survives somebody raising ReadyTimeout later without
// thinking about the queue.
func (w *Worker) heartbeat(ctx context.Context, org string) func() {
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		defer func() {
			// The five-line shape every worker goroutine in cmd/server uses: a
			// panic here would otherwise take the process down mid-provision
			// and leave the organisation claimed until its lease expired.
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
					// can mean the claim is already gone — which is the one
					// condition under which two provisioners build the same
					// organisation. Loud, and counted.
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

// record writes the organisation to both databases, in the order that makes a
// crash between them safe.
//
// The console row first, then cloud.orgs.console_url. /authorize reads
// ConsoleURL only after membership, and an empty one is what raises
// ErrNoConsole — so console_url is the commit point: non-empty implies the
// console row already exists.
//
// `cloud org register` writes them the other way round and is also correct,
// because its hazard is the mirror image: it can be run for an organisation
// Cloud has never heard of. Here the foreign key on cloud.org_provisioning
// means the Cloud row exists before anything is ever claimed. If these two
// paths are ever unified, one of them regresses.
func (w *Worker) record(ctx context.Context, org string, st provision.Status) error {
	reg := console.OrgRegistration{
		Org:        org,
		Endpoint:   st.Endpoint,
		HealthAddr: st.HealthAddr,
		CAPEM:      string(st.CAPEM),
		CertPEM:    string(st.ConsoleCertPEM),

		// []byte on both sides. The private keys are the two fields that are
		// NOT a []byte-to-string conversion, and wrapping them would not
		// compile — which is the one thing in this mapping that cannot go
		// wrong silently.
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
	return nil
}

// publicEndpoint reports what callers dial, when that differs from what the
// console dials.
//
// Ensure sets PublicEndpoint equal to Endpoint in a single-network deployment,
// which is every deployment today. Copying that through would write an
// atl_public_endpoint duplicating atl_endpoint — exactly what that column's own
// comment says not to do, because the read path is a COALESCE fallback that is
// deliberately not backfilled. Storing the duplicate retires the fallback
// silently, and the two then diverge the first time somebody changes Endpoint
// alone.
func publicEndpoint(st provision.Status) string {
	if st.PublicEndpoint == st.Endpoint {
		return ""
	}
	return st.PublicEndpoint
}

// fail records an attempt that did not finish, and schedules the next one.
func (w *Worker) fail(ctx context.Context, c store.Claimed, cause error) {
	// A cancelled context is a shutdown, not the organisation's fault, so it
	// does not escalate the backoff. The attempt is still recorded as failed
	// because it is: nothing is serving, and the row must be claimable again.
	retryIn := w.cfg.backoff(c.Attempts)
	switch {
	case ctx.Err() != nil:
		// A shutdown is not the organisation's fault, so it does not escalate
		// the backoff. The attempt is still recorded as failed because it is:
		// nothing is serving, and the row must be claimable again.
		retryIn = w.cfg.RetryBase
		cause = fmt.Errorf("the provisioner shut down mid-attempt: %w", cause)
	case credentialError(cause):
		// Nor is this. Escalating here would punish an organisation for a
		// platform fault and, with several queued, would push all of them into
		// long backoffs while the actual problem — credentials — went unnamed.
		retryIn = w.cfg.RetryBase
		cause = fmt.Errorf("the cluster refused this provisioner's credentials, "+
			"which is a fault in the provisioner and not in this organisation: %w", cause)
	}

	w.log.Error("provisioning did not finish",
		"org", c.Org, "attempt", c.Attempts, "retry_in", retryIn, "err", cause)

	// Detached from ctx on purpose. The cause is often that ctx was cancelled,
	// and a mark issued on a cancelled context does not land — which would
	// leave the row claimed until the lease expired, turning a clean restart
	// into a wait.
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
// The actor is a named constant rather than an empty string: a blank actor
// reads as a bug in the logging rather than as a machine acting. There is no
// actor email because there is no human — this is the same convention
// enrolmentActor follows in the console.
//
// LogAction returns nothing and logs its own failures, which is deliberate: an
// audit write that fails must not fail the provisioning, and must not be
// silently discarded either.
func (w *Worker) audit(ctx context.Context, org, action string, detail map[string]any) {
	w.q.LogAction(ctx, org, store.ProvisionerActor, "", action, detail)
}

// reap destroys the organisations whose retention window has run out.
//
// # Why this runs on the reconcile tick and not the poll tick
//
// Deletion is not urgent. An organisation deleted thirty days ago can wait
// another five minutes, and running this every ten seconds would mean a query
// against the whole queue table at that rate for work that appears a handful of
// times a month.
//
// rotateConsole replaces one organisation's console credentials when they are
// near expiry, or when an operator has asked.
//
// # Why a failure here is logged and not escalated
//
// Nothing about the organisation is wrong. Its pods are running, its callers are
// authenticating, and the only thing that has not happened is a credential
// replacement that will be retried on the next pass. Requeueing would rebuild a
// healthy organisation — and rebuilding mints a new authority, which is exactly
// the outage this whole mechanism exists to avoid. Marking it failed would take
// a serving organisation out of `ready` for a reason its customers cannot see.
//
// What does need to be loud is the credential running out while these failures
// repeat, and that is what the metric below is for: a rotation that has been
// failing for days is invisible in logs nobody reads and obvious in a gauge.
//
// # Why the request is cleared only after the registration succeeds
//
// The operator asked for the console to be using a different certificate. That
// is only true once the console has been told about it — a rotation written to
// the cluster and never registered leaves the console presenting the old
// credential, which still works, so nothing would look wrong.
// It returns when this organisation's console credential expires, or the zero
// time when the pass could not find out. The caller aggregates those into the
// fleet-wide gauge; see reconcile.
func (w *Worker) rotateConsole(
	ctx context.Context, c Cluster, org string, requestedAt time.Time, requested bool,
) time.Time {
	r, err := c.RotateConsoleCredentials(
		ctx, org, w.cfg.ConsoleCertRenewWithin, requested)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		// The expiry is carried out of every failure that got far enough to read
		// a certificate, and it is reported here rather than dropped. A rotation
		// that keeps failing is the one case this whole mechanism exists to make
		// visible, and discarding what it learned would leave the fleet gauge
		// frozen at the last healthy reading while the credential ran out.
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
		// Nothing was due, so this credential is comfortably in date by
		// definition. No warning: reaching the window is what makes one due, so
		// a healthy pass cannot produce one worth warning about.
		return r.ExpiresAt
	}

	// Re-registering is what puts the new certificate into use. The console
	// notices within orgClientRefresh because the row's updated_at moves.
	if err := w.record(ctx, org, r.Status); err != nil {
		consoleRotationFailures.Inc()
		w.log.Error("rotated an organisation's console credentials but could not "+
			"register them; the console keeps using the previous certificate "+
			"until this succeeds", "org", org, "err", err)
		// The previous certificate is the one still in use, so that is the expiry
		// that describes this organisation — not the new one, which nothing is
		// presenting yet. Reporting the new one here would show the credential as
		// renewed at the exact moment renewal stopped taking effect.
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
// The metric is the thing to alert on, but a log line is what the person who
// gets paged reads next, and "which organisation" is the question they will
// have. The gauge is a fleet minimum and cannot answer it.
//
// # Where this is called from, and where it deliberately is not
//
// Only the failure paths: a rotation that errored, and one written to the
// cluster that the console was never told about. Those are the two ways a
// credential inside the window survives a pass.
//
// It was first called on the not-rotated path instead, where it could never
// fire — reaching the window is what makes a credential due, so a pass that
// rotated nothing had nothing to warn about by construction. A warning that
// cannot fire is worse than none, because the silence reads as evidence.
func (w *Worker) warnIfExpiringSoon(org string, expires time.Time) {
	if expires.IsZero() {
		return
	}
	left := time.Until(expires)
	if left > w.cfg.ConsoleCertRenewWithin {
		return
	}
	w.log.Warn("an organisation's console credential is inside the renewal window "+
		"and was not rotated; when it expires, nobody can open this organisation "+
		"in a browser",
		"org", org,
		"expires", expires.Format(time.RFC3339),
		"remaining", left.Round(time.Hour).String())
}

// # The most dangerous loop in the system
//
// It destroys customer data on a timer with no human in it. Everything that
// makes that safe is in store.ClaimForPurge's predicate rather than here: a row
// is only claimable when its state is `deleted` and its purge_after has passed,
// or when it is already `purging` under a lease that expired. This function
// trusts that entirely and does not second-guess it — a second opinion here
// would be a second place for the rule to be wrong.
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
// The order is the whole design: the cluster first, the database second. A
// crash between them leaves the namespace gone and the row still reading
// `purging`, which the next pass reclaims and retries — and Destroy on an
// absent namespace succeeds, so the retry converges.
//
// The other order would delete the row that says which namespace to destroy,
// stranding a live namespace with a customer's data in it and nothing left
// pointing at it.
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
		// Released rather than marked terminally failed. A customer who asked
		// to be deleted and quietly was not is the worst outcome here, so this
		// keeps being retried rather than backing off into silence.
		log.Error("could not destroy the organisation", "err", err)
		if mErr := w.q.MarkPurgeFailed(ctx, org, err.Error()); mErr != nil {
			log.Error("could not release the purge claim", "err", mErr)
		}
		return
	}

	// The console's copy, which the destroyed namespace does not take with it.
	//
	// It holds a sealed private key for this organisation, plus the sessions,
	// enrolment tokens and certificate fingerprints that referenced it — none of
	// which means anything now that the authority behind them is gone. Left
	// behind, they accumulate one dead organisation at a time while `cloud org
	// purge` reports that everything was destroyed.
	//
	// Between Destroy and MarkPurged on purpose. Before Destroy it would strip a
	// live organisation's credentials and then possibly fail to destroy it,
	// leaving something running that the console can no longer reach. After
	// MarkPurged the row would be gone and nothing would retry this.
	if err := w.unregister(ctx, w.cfg.ConsolePGURL, org); err != nil {
		// Retried rather than logged past. The claim is released so the next
		// pass picks it up; Destroy is idempotent against an absent namespace,
		// so the retry converges instead of repeating work.
		log.Error("the organisation was destroyed but the console still holds "+
			"its credentials", "err", err)
		if mErr := w.q.MarkPurgeFailed(ctx, org, err.Error()); mErr != nil {
			log.Error("could not release the purge claim", "err", mErr)
		}
		return
	}

	if err := w.q.MarkPurged(ctx, org); err != nil {
		// The namespace is gone and the rows are not. The next pass reclaims
		// the row, Destroy succeeds against an absent namespace, and it
		// converges — which is why this is a log and not a failure mark.
		log.Error("the organisation was destroyed but its rows remain", "err", err)
		return
	}

	outcome = "purged"
	log.Info("purged", "took", time.Since(start).Round(time.Second))

	// Audited before the rows go, or rather: audited knowing they have. The
	// audit log references the organisation by name and outlives it, which is
	// the point — "what happened to acme" must still have an answer after acme
	// stops existing.
	w.audit(ctx, org, "org.purged", map[string]any{
		"took_ms": time.Since(start).Milliseconds(),
	})
}
