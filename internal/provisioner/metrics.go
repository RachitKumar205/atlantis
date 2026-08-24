// Observability for provisioning. Same shape as the other obs subsystems
// (internal/obs/metrics.go, jobs/metrics.go): package-level promauto vars
// registered against the default registerer at import time, with
// bounded-cardinality labels.
//
// # Why this package declares its own rather than importing internal/obs
//
// Because /metrics on this binary would otherwise be empty, and empty in a way
// that looks wired. internal/obs registers at import time, but nothing in this
// process's import graph reaches it: neither internal/cloud/store nor
// internal/cloud/provision pulls in prometheus at all. Mounting
// promhttp.Handler() here without declaring anything serves go_* and process_*
// and no atlantis series whatsoever.
//
// Importing internal/obs to fix that is the opposite mistake. It would register
// twelve server-shaped collectors this process never increments — a permanent
// row of zeros that reads as a broken server rather than as a worker with a
// surface of its own.

package provisioner

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// attemptsTotal counts provisioning attempts by how they ended.
	//
	// The distinction this exists to make is the one a log line cannot: an
	// idle queue and a queue whose head has been failing since Tuesday both
	// produce no output at all, because a provisioner with nothing to do says
	// nothing and a provisioner failing the same organisation says the same
	// thing every time.
	//
	// The label is a closed set of three, chosen here rather than read from
	// the database. A label taken from a column is not bounded — the reason
	// NormalizeOutboxKind exists — and "outcome" is exactly the kind of field
	// somebody later adds a state to.
	attemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "attempts_total",
		Help:      "Provisioning attempts. Labels: outcome (provisioned|failed|not_ready).",
	}, []string{"outcome"})

	// purgesTotal counts teardowns after a retention window has run out.
	//
	// Separate from attemptsTotal rather than another outcome on it, because
	// the two answer different questions and mixing them would make both
	// useless: "how much provisioning is failing" is a rate an operator watches
	// continuously, and a purge is a rare, deliberate, irreversible act.
	//
	// A rising `failed` here is the alarming one on this counter. It means
	// organisations whose owners asked for deletion are not being deleted, and
	// there is no backoff on that path — a customer who asked to be removed and
	// quietly was not is the failure this number exists to make visible.
	purgesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "purges_total",
		Help:      "Organisation teardowns after the retention window. Labels: outcome (purged|failed).",
	}, []string{"outcome"})

	// attemptSeconds is how long an attempt took, whatever the outcome.
	//
	// Measured around the whole claim-to-mark span rather than around Ensure,
	// because the number an operator wants is "how long does an organisation
	// wait", and most of that is initdb rather than anything this code does.
	//
	// Buckets run to ~8 minutes: a first provision on a cold cluster was
	// measured at ~48 seconds, and the default ReadyTimeout is five minutes, so
	// the interesting range is wider than prometheus.DefBuckets covers.
	attemptSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "attempt_duration_seconds",
		Help:      "Wall time from claim to mark. Labels: outcome (provisioned|failed|not_ready).",
		Buckets:   []float64{5, 15, 30, 60, 120, 240, 480},
	}, []string{"outcome"})

	// claimFailuresTotal counts errors reaching the queue at all.
	//
	// Separate from a failed attempt, and the separation matters: an
	// organisation that will not provision is a customer problem, while a
	// provisioner that cannot read its queue is an outage that provisions
	// nobody and — without this — increments nothing.
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
	// customer's namespace outside this system — or that the cluster was
	// rebuilt — and every caller certificate in those organisations is about to
	// be invalidated, because the authority went with the namespace. That is
	// worth alerting on rather than reading about afterwards.
	reconciledTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "reconciled_total",
		Help:      "Ready organisations found absent from the cluster and requeued.",
	})

	// consoleRotationsTotal counts console credentials actually replaced.
	//
	// Expected to be quiet, and to move in step with the renewal window rather
	// than with the reconcile interval. A rate close to one per organisation per
	// pass means the "is it due" test is answering yes every time — a rotation
	// loop, which is harmless to customers and burns through certificates while
	// looking like the feature working.
	consoleRotationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "console_rotations_total",
		Help:      "Console credentials reissued and re-registered.",
	})

	// consoleRotationFailures counts rotations that did not complete.
	//
	// This is the one that has to be alerted on, and the reason it exists at all.
	// A failing rotation changes nothing an operator or a customer can see: the
	// organisation stays ready, its pods stay up, its callers keep working, and
	// the console keeps using a certificate that is running down. The failure is
	// silent right up to the moment the credential expires and every browser
	// loses that organisation at once.
	//
	// So the alarm has to come from here rather than from the outage. With a
	// renewal window of ten days, a sustained non-zero value is a warning with
	// more than a week of margin on it; ignored, it becomes a fleet-wide
	// incident with no proximate cause.
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
	// building the same organisation. It should be zero, and a non-zero value
	// is worth looking at even though the work usually still completes.
	leaseExtensionFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "provisioning",
		Name:      "lease_extension_failures_total",
		Help:      "Lease heartbeats that failed, meaning the claim may have been lost.",
	})
)
