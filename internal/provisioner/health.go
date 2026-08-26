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

// NewHealthServer wires /healthz and /readyz. /metrics is on a separate
// listener.
//
// This one must be open: the kubelet probes it holding no credential, and
// tenant namespaces restrict ingress rather than egress, so every pod in the
// cluster can reach the port. NewMetricsServer binds where only this pod can
// reach.
//
// Once shutdownSignal cancels, /readyz answers 503 immediately.
func NewHealthServer(addr string, ping func(context.Context) error, shutdownSignal context.Context) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", readyHandler(ping, shutdownSignal))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// NewMetricsServer serves /metrics alone, on its own address.
//
// The address comes from the caller and is expected to be loopback: a value
// hard-coded here could not be overridden the day something does scrape this.
// See DefaultMetricsAddr.
func NewMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// readyHandler reports whether this provisioner can do its job.
//
// Its job is reading a queue, so the queue is the dependency. Kubernetes is not
// probed: a briefly unreachable cluster produces a failed attempt with a
// backoff, which is handled, rather than a process that should stop being
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
