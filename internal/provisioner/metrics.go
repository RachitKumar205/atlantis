// Observability for provisioning. Same shape as internal/obs/metrics.go and
// jobs/metrics.go: package-level promauto vars registered at import time.
//
// Declared here rather than imported from internal/obs, which this process's
// import graph does not reach, and whose collectors are server-shaped and would
// register unincremented.

package provisioner

import (
	"math"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// markConsoleCertUnmeasured sets consoleCertSecondsLeft to NaN.
//
// A gauge defaults to zero, which on that metric means "expiring now" and would
// page on every start. NaN reads as no measurement.
func markConsoleCertUnmeasured() {
	consoleCertSecondsLeft.Set(math.NaN())
}

func init() {
	markConsoleCertUnmeasured()
}

var (
	// attemptsTotal counts provisioning attempts by how they ended. An idle
	// queue and a queue failing on its head both produce no log output.
	//
	// The outcome label is a closed set of three, fixed here rather than read
	// from a column, which would be unbounded cardinality.
	attemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "attempts_total",
		Help:      "Provisioning attempts. Labels: outcome (provisioned|failed|not_ready).",
	}, []string{"outcome"})

	// purgesTotal counts teardowns after a retention window has run out.
	// Separate from attemptsTotal: provisioning failure is a continuous rate,
	// and a purge is rare and irreversible.
	//
	// A rising `failed` means organisations asked to be deleted are not being
	// deleted. There is no backoff on that path.
	purgesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "purges_total",
		Help:      "Organisation teardowns after the retention window. Labels: outcome (purged|failed).",
	}, []string{"outcome"})

	// attemptSeconds is how long an attempt took, whatever the outcome.
	// Measured claim-to-mark rather than around Ensure, since most of the wait
	// is initdb.
	//
	// Buckets run to 8 minutes. A first provision on a cold cluster measured
	// ~48s and the default ReadyTimeout is five minutes, both past
	// prometheus.DefBuckets.
	attemptSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "attempt_duration_seconds",
		Help:      "Wall time from claim to mark. Labels: outcome (provisioned|failed|not_ready).",
		Buckets:   []float64{5, 15, 30, 60, 120, 240, 480},
	}, []string{"outcome"})

	// claimFailuresTotal counts errors reaching the queue at all.
	//
	// Separate from a failed attempt: a provisioner that cannot read its queue
	// provisions none of them and would otherwise increment nothing.
	claimFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "claim_failures_total",
		Help:      "Failures querying the provisioning queue.",
	})

	// reconciledTotal counts organisations put back in the queue because the
	// cluster no longer had them.
	//
	// Expected to be zero. A non-zero value means something removed a
	// customer's namespace outside this system, or the cluster was rebuilt, and
	// every caller certificate in those organisations is about to be
	// invalidated because the authority went with the namespace.
	reconciledTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "reconciled_total",
		Help:      "Ready organisations found absent from the cluster and requeued.",
	})

	// consoleRotationsTotal counts console credentials actually replaced.
	//
	// Expected to be quiet, and to move in step with the renewal window rather
	// than with the reconcile interval. A rate close to one per organisation
	// per pass means the "is it due" test is answering yes every time.
	consoleRotationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "console_rotations_total",
		Help:      "Console credentials reissued and re-registered.",
	})

	// consoleCertSecondsLeft is how long the soonest-expiring console credential
	// in the fleet has left. Alert well above ConsoleCertRenewWithin.
	//
	// init below sets it to NaN. A gauge defaults to zero, which here reads as
	// expiring now, so every threshold rule would fire until the first
	// reconcile pass. Comparisons against NaN are false.
	consoleCertSecondsLeft = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "console_cert_seconds_left",
		Help:      "Seconds until the soonest-expiring console credential in the fleet expires. NaN before the first reconcile pass.",
	})

	// consoleRotationFailures counts rotations that did not complete. A failing
	// rotation is otherwise invisible: the organisation stays ready and the
	// console keeps using a certificate running down, until it expires and
	// every browser loses that organisation at once.
	consoleRotationFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "console_rotation_failures_total",
		Help:      "Console credential rotations that failed; sustained non-zero means an expiry is coming.",
	})

	// leaseExtensionFailuresTotal counts heartbeats that did not land.
	//
	// A failure here means this process no longer owns the row it is working
	// on, which is the one condition under which two provisioners can be
	// building the same organisation. Expected to be zero; the work in progress
	// usually still completes.
	leaseExtensionFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "lease_extension_failures_total",
		Help:      "Lease heartbeats that failed, meaning the claim may have been lost.",
	})
)
