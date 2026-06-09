// Observability for the worker-dispatcher. Same shape as the existing
// obs subsystems (internal/obs/metrics.go): package-level promauto
// vars registered against the default registerer at import time, with
// bounded-cardinality labels (queue + job_name + caller, all closed
// sets at the deployment level).

package jobsdispatcher

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// dispatchedTotal counts jobs the dispatcher has pushed to a worker
	// session. Counted at the moment the Dispatch envelope reaches the
	// session outbox; not waiting for Ack so the gauge of "what we
	// intended to dispatch" doesn't lag the actual workload.
	dispatchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "jobs_dispatched_total",
		Help:      "Jobs the dispatcher pushed to a worker session. Labels: queue, job_name.",
	}, []string{"queue", "job_name"})

	// completedTotal counts terminal-success Complete envelopes.
	completedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "jobs_completed_total",
		Help:      "Jobs the dispatcher saw complete successfully. Labels: queue, job_name.",
	}, []string{"queue", "job_name"})

	// failedTotal counts terminal-fail or transient-fail envelopes.
	// The terminal label distinguishes "row moved to DLQ" (true) from
	// "row reset to pending" (false) so the operator can spot DLQ
	// growth without correlating against ReportFailure logs.
	failedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "jobs_failed_total",
		Help:      "Jobs the dispatcher saw fail. Labels: queue, job_name, terminal (true=DLQ, false=retry).",
	}, []string{"queue", "job_name", "terminal"})

	// revokedTotal counts rows the server pulled back from a worker
	// (lease expiry, session close, operator action, timeout).
	revokedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "jobs_revoked_total",
		Help:      "Jobs the dispatcher revoked. Labels: queue, reason.",
	}, []string{"queue", "reason"})

	// sessionsActive is the current count of connected worker sessions
	// per queue+caller. Useful for "is anyone listening on vendor?"
	// dashboards.
	sessionsActive = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "sessions_active",
		Help:      "Currently connected worker sessions. Labels: queue, caller.",
	}, []string{"queue", "caller"})

	// inflightGauge is the current in-flight count per session. The
	// session_id label is cardinality-stable per session connect/
	// disconnect; an operator watching this at the prometheus
	// instance level sees one series per active session at any time.
	inflightGauge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "inflight",
		Help:      "In-flight jobs per session. Labels: queue, session_id.",
	}, []string{"queue", "session_id"})

	// dispatchLatency measures time from claim to Ack receipt. Buckets
	// tuned for sub-second-typical handler startup (LAN gRPC + handler
	// goroutine spawn).
	dispatchLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "atlantis",
		Subsystem: "dispatcher",
		Name:      "dispatch_latency_seconds",
		Help:      "Time from claim to Ack receipt. Labels: queue.",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"queue"})
)
