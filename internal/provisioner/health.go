package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// probeTimeout bounds the readiness check so a wedged database cannot hold a
// probe open for the whole request.
const probeTimeout = 3 * time.Second

// NewHealthServer wires /healthz, /readyz and /metrics.
//
// A worker with no HTTP surface at all is invisible in Kubernetes: nothing can
// restart it when it wedges, nothing can scrape it, and its only signal is a
// log line — which is precisely the state jobs/metrics.go records having been
// bitten by, where "a reap that found nothing to do and a reap where fifty
// objects failed produced identical output".
//
// Reduced from cmd/server/health.go rather than copied: there is no /status,
// because nothing renders a Health page for this process, and the readiness
// check is one dependency rather than three.
//
// Once shutdownSignal cancels, /readyz answers 503 immediately, so an
// orchestrator stops routing to this pod before the process itself goes away.
func NewHealthServer(addr string, ping func(context.Context) error, shutdownSignal context.Context) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", readyHandler(ping, shutdownSignal))
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// readyHandler reports whether this provisioner can do its job.
//
// Its job is reading a queue, so the queue is the dependency. Kubernetes is
// deliberately not probed: the provisioner is useful the moment it can claim,
// and a cluster that is briefly unreachable produces a failed attempt with a
// backoff — which is handled — rather than a process that should stop being
// scheduled.
func readyHandler(ping func(context.Context) error, shutdownSignal context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if shutdownSignal != nil && shutdownSignal.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		if ping == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintln(w, "ok")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "cloud db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	}
}
