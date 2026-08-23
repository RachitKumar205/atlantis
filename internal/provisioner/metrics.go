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
