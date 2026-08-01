// Observability for the built-in jobs. Same shape as the other obs subsystems
// (internal/obs/metrics.go, internal/server/jobsdispatcher/metrics.go):
// package-level promauto vars registered against the default registerer at
// import time, with bounded-cardinality labels.

package jobs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// reapedTotal counts objects the reaper actually dropped.
	//
	// The whole reason these exist: a reap that found nothing to do and a reap
	// where fifty objects failed to drop produced identical output — one INFO
	// line at the top of the hour, or none. Nothing distinguished "the system
	// is idle" from "the system has been failing since Tuesday", which is the
	// distinction an operator needs and the one a log line at that volume
	// cannot carry.
	reapedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "reaper",
		Name:      "objects_reaped_total",
		Help:      "Parked objects dropped after their retention window. Labels: kind (table|column).",
	}, []string{"kind"})

	// reapFailuresTotal counts drops that were declined or refused.
	//
	// Expected to be non-zero occasionally — a lock the reap declined to wait
	// for is a success of the design, not a fault — so this is worth alerting
	// on as a sustained rate rather than a single event.
	reapFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "reaper",
		Name:      "reap_failures_total",
		Help:      "Reap attempts that failed. Labels: kind (table|column).",
	}, []string{"kind"})

	// parkedPending is how many objects are registered and not yet reaped.
	//
	// A gauge rather than a counter because the question is "how much is
	// outstanding right now". Sustained growth means reaping is not keeping
	// up — or, more likely, is failing — and it is the single number that
	// makes a stuck reaper visible without reading logs.
	parkedPending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "reaper",
		Name:      "objects_pending",
		Help:      "Parked objects registered and not yet reaped.",
	})

	// parkedDue is the subset of pending objects whose window has passed.
	//
	// Separate from parkedPending because they answer different questions.
	// Pending is expected to be large and to fluctuate; due should return to
	// zero after every run, and a due count that never falls is exactly the
	// starvation the backoff column exists to prevent.
	parkedDue = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "reaper",
		Name:      "objects_due",
		Help:      "Parked objects whose retention window has passed and that have not been reaped.",
	})

	// sweptTotal counts rows the TTL sweeper deleted.
	sweptTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "sweeper",
		Name:      "rows_swept_total",
		Help:      "Expired rows deleted by the TTL sweeper. Labels: entity.",
	}, []string{"entity"})

	// scheduleFiresTotal counts jobs the scheduler enqueued.
	//
	// Labelled by outcome so "fired" and "skipped because the previous run is
	// still outstanding" are distinguishable. A schedule that only ever skips
	// is a wedged handler, and it looks like a healthy schedule from every
	// other angle.
	scheduleFiresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "scheduler",
		Name:      "fires_total",
		Help:      "Scheduled job evaluations that produced an outcome. Labels: job, outcome (fired|skipped).",
	}, []string{"job", "outcome"})
)
