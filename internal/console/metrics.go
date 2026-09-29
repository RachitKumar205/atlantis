// Observability for the fleet sweep.
//
// The collectors are registered in fleetRegistry, which only the console's
// metrics listener serves. internal/provisioner imports this package, and its
// /metrics serves the default registry.
//
// Importing internal/obs would register its pool, cache and outbox collectors,
// unincremented, in a process that has none of them.

package console

import (
	"math"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// markFleetUnmeasured sets fleetOrgs to NaN.
//
// A gauge defaults to zero, and zero organisations is a real and alarming
// state. NaN reads as no measurement, and every threshold comparison against it
// is false.
func markFleetUnmeasured() {
	fleetOrgs.Set(math.NaN())
}

func init() {
	markFleetUnmeasured()
}

// fleetRegistry holds every fleet collector; fleetFactory registers into it.
var (
	fleetRegistry = prometheus.NewRegistry()
	fleetFactory  = promauto.With(fleetRegistry)
)

var (
	// fleetReachable is whether the last sweep reached each organisation's
	// health listener. The reason a zero has is in console.org_facts; a label
	// for it here would leave the previous reason's child at 1 when the reason
	// changed.
	fleetReachable = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "reachable",
		Help:      "1 when the last sweep reached this organisation, 0 when it did not. The reason is in console.org_facts.",
	}, []string{"org"})

	// fleetSchemaVersion is the highest schema version each organisation has
	// applied. The fleet spread is what says a deploy reached some tenants.
	fleetSchemaVersion = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "schema_version",
		Help:      "Highest applied schema version. NaN when the last sweep could not read it.",
	}, []string{"org"})

	// fleetDeadJobs counts what has stopped retrying. Rising means work is
	// being dropped, which nothing else in the fleet reports.
	fleetDeadJobs = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "dead_jobs",
		Help:      "Dead-letter jobs, capped at the sweep's limit. NaN when the last sweep could not read them.",
	}, []string{"org"})

	// fleetParkedObjects counts what a destructive migration renamed out of the
	// way and has not reaped.
	fleetParkedObjects = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "parked_objects",
		Help:      "Parked objects not yet reaped, capped at the sweep's limit. NaN when unread.",
	}, []string{"org"})

	// fleetParkedOverdue counts parked objects whose retention has run out and
	// which are still there, which is what the register is read for.
	//
	// Counted over the same page as fleetParkedObjects, so it is a lower bound
	// whenever that one is, and counts_truncated{what="parked_objects"} covers
	// both. The page is ordered by reap_after, earliest first, so the overdue
	// rows are the ones a truncated page keeps.
	fleetParkedOverdue = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "parked_objects_overdue",
		Help:      "Parked objects whose reap_after has passed and which are still present. A lower bound when counts_truncated{what=\"parked_objects\"} is 1. NaN when unread.",
	}, []string{"org"})

	// fleetCountsTruncated says a count is a lower bound.
	//
	// ListDeadJobs carries no total and no has_more, so a response filling the
	// sweep's limit and a queue exactly that long are the same message. Without
	// this a capped count reads as the whole queue.
	fleetCountsTruncated = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "counts_truncated",
		Help:      "1 when the sweep hit its limit and the matching count is a lower bound. Labels: what (dead_jobs|parked_objects).",
	}, []string{"org", "what"})

	// fleetFreezeOpen is whether a freeze window covers now. Computed by the
	// sweep: ListFreezeWindows returns every row, including expired ones, and
	// has no notion of the current time.
	fleetFreezeOpen = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "freeze_window_open",
		Help:      "1 when a freeze window covers now. NaN when the last sweep could not read them.",
	}, []string{"org"})

	// fleetLastPoll is when each organisation was last visited. Alert on the
	// age: a sweep that has stopped leaves every other gauge at its last value,
	// and this is the only one that says so.
	fleetLastPoll = fleetFactory.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "last_poll_timestamp_seconds",
		Help:      "Unix time of the last sweep that visited this organisation.",
	}, []string{"org"})

	// fleetOrgs is how many organisations the last sweep visited. init sets it
	// to NaN; see markFleetUnmeasured.
	fleetOrgs = fleetFactory.NewGauge(prometheus.GaugeOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "orgs",
		Help:      "Organisations visited by the last sweep. NaN before the first.",
	})

	// fleetSweepFailures counts sweeps, and writes within them, that failed.
	//
	// A sweep that cannot list organisations publishes nothing and retires
	// nothing, so every per-organisation gauge keeps its last value. Without
	// this a console whose own database is down reports a frozen but healthy
	// fleet, and looks the same as a console whose sweep has stopped.
	//
	// The stage label is a closed set of two.
	fleetSweepFailures = fleetFactory.NewCounterVec(prometheus.CounterOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "sweep_failures_total",
		Help:      "Sweep steps that failed. Labels: stage (list|write).",
	}, []string{"stage"})

	// fleetSweepSeconds is how long a whole sweep took. Buckets run to five
	// minutes, the default interval: a sweep approaching its own interval is
	// the signal that the concurrency or the interval has to move.
	fleetSweepSeconds = fleetFactory.NewHistogram(prometheus.HistogramOpts{
		Namespace: "atlantis",
		Subsystem: "console_fleet",
		Name:      "sweep_duration_seconds",
		Help:      "Wall time of one whole sweep.",
		Buckets:   []float64{1, 5, 15, 30, 60, 120, 300},
	})
)

// orgGauges is every per-organisation collector, for retiring a label set.
var orgGauges = []*prometheus.GaugeVec{
	fleetReachable, fleetSchemaVersion, fleetDeadJobs,
	fleetParkedObjects, fleetParkedOverdue, fleetFreezeOpen, fleetLastPoll,
}

// publishOrgFacts writes one organisation's gauges.
//
// A fact the sweep could not measure is NaN, not zero. Zero means measured and
// none: a new organisation genuinely has schema version 0 and no dead jobs, and
// an organisation whose admin plane refused the call must not read the same.
func publishOrgFacts(f orgFacts) {
	reachable := 0.0
	if f.Reachable {
		reachable = 1
	}
	fleetReachable.WithLabelValues(f.Org).Set(reachable)
	fleetLastPoll.WithLabelValues(f.Org).Set(float64(f.CollectedAt.Unix()))

	setOrNaN(fleetSchemaVersion, f.Org, i64(f.SchemaVersion))
	setOrNaN(fleetDeadJobs, f.Org, i32(f.DeadJobs))
	setOrNaN(fleetParkedObjects, f.Org, i32(f.ParkedObjects))
	setOrNaN(fleetParkedOverdue, f.Org, i32(f.ParkedOverdue))
	setOrNaN(fleetFreezeOpen, f.Org, boolean(f.FreezeOpen))

	truncated(f.Org, "dead_jobs", f.DeadJobsTruncated)
	truncated(f.Org, "parked_objects", f.ParkedTruncated)
}

func truncated(org, what string, v *bool) {
	if v == nil {
		fleetCountsTruncated.DeleteLabelValues(org, what)
		return
	}
	n := 0.0
	if *v {
		n = 1
	}
	fleetCountsTruncated.WithLabelValues(org, what).Set(n)
}

func setOrNaN(g *prometheus.GaugeVec, org string, v *float64) {
	if v == nil {
		g.WithLabelValues(org).Set(math.NaN())
		return
	}
	g.WithLabelValues(org).Set(*v)
}

func i64(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func i32(v *int32) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

func boolean(v *bool) *float64 {
	if v == nil {
		return nil
	}
	f := 0.0
	if *v {
		f = 1
	}
	return &f
}

// retireOrgGauges drops the children of organisations the sweep did not visit.
//
// GaugeVec children are never collected, so an organisation removed from the
// registry keeps publishing its last values and an alert on it never clears.
//
// Called after the sweep with the complete set it saw, rather than Reset before
// it. Reset clears every child and the sweep repopulates them one organisation
// at a time over minutes, so a scrape landing in between sees live
// organisations reporting nothing — which is absent rather than zero, and an
// alert on reachable == 0 goes blind exactly when it should fire.
func retireOrgGauges(published map[string]bool, seen map[string]bool) {
	for org := range published {
		if seen[org] {
			continue
		}
		for _, g := range orgGauges {
			g.DeleteLabelValues(org)
		}
		fleetCountsTruncated.DeletePartialMatch(prometheus.Labels{"org": org})
	}
}

// newMetricsServer serves /metrics alone, on its own address.
//
// A second listener because the console's own mux is browser-facing behind a
// TLS terminator with cookie auth: a scraper holds no session, and the
// alternative is an unauthenticated route on a public hostname. cmd/server
// splits the two for the same reason.
//
// No client certificate. The console verifies each organisation's callers
// against that organisation's own authority, and has no process-wide client CA
// to check one against — CONSOLE_ENROLL_CLIENT_CA was removed because one pool
// could not verify callers from many authorities. The loopback bind is what
// stands in for it.
//
// The address is expected to be loopback and comes from the caller, so a value
// fixed here could not be overridden the day something scrapes this. These
// series carry the customer list, each customer's schema version and each
// customer's dead-job backlog.
func newMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		prometheus.Gatherers{prometheus.DefaultGatherer, fleetRegistry}, promhttp.HandlerOpts{}))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
