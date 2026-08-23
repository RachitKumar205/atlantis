package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/cache/memcached"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// healthDeps is the process state the readiness handler inspects on
// each /readyz call. Liveness has no dependencies — the process being
// up is enough.
type healthDeps struct {
	Pool   *pgxpool.Pool
	MC     *memcached.Client
	Worker *invalidate.Worker

	// WorkerMaxStaleness caps the age of Worker.LastDrainAt before
	// /readyz fails. Sized at ~3x the drain interval to absorb jitter
	// without false negatives.
	WorkerMaxStaleness time.Duration

	// ProbeTimeout bounds each dependency check on a single /readyz
	// call so a wedged dep cannot block a probe for the full request.
	ProbeTimeout time.Duration

	// StartedAt is the boot time of this process; used by /status to
	// report uptime to the console Health page.
	StartedAt time.Time

	// Version is the linker-stamped server version (main.version),
	// exposed via /status for the same reason.
	Version string
}

// newHealthServer wires the four health routes on addr, over TLS, with two
// different trust levels.
//
// # Why this listener has a credential at all
//
// It used to have none, and the comment on statusHandler explained why: these
// routes "live on the same port as /healthz and /metrics which are already
// exposed at the platform layer". That was a reasonable position for a
// single-tenant deployment, where every workload in the cluster belongs to one
// operator and a NetworkPolicy is a sufficient boundary.
//
// It is not this deployment. Organisations are provisioned into a shared
// cluster and run their customers' SQL, so a neighbouring pod is somebody
// else's program. The only thing that kept it out was a NetworkPolicy — and
// that policy is not portable: `ipBlock` covers Pod traffic under Calico, never
// covers it under GKE Dataplane V2, and on EKS the exclusion matches nothing at
// all because Pods take VPC addresses. On one of those the isolation silently
// stops existing while every manifest still says it is there.
//
// So the network is no longer the boundary. A credential is, and the policy
// becomes a second layer whose inconsistency is tolerable.
//
// # Why one listener rather than two
//
// The routes carry two trust levels. /healthz and /readyz disclose nothing and
// must stay reachable by the kubelet, which presents no certificate. /status
// gives the schema and build version; /metrics gives per-caller RPC counts.
// Those describe a customer's system.
//
// Two ports would express that split, at the cost of a second Service port, a
// second stored address, and a migration to carry it. One listener with
// VerifyClientCertIfGiven expresses the same thing: an absent certificate is
// allowed onto the open routes, a presented one is verified against the same CA
// the admin plane uses, and the closed routes require that verification to have
// happened. The distinction is enforced per route by credential rather than by
// port number.
//
// Once shutdownSignal cancels, /readyz returns 503 immediately so the load
// balancer drains the pod before the HTTP server itself shuts down.
func newHealthServer(addr string, deps healthDeps, tlsCfg *tls.Config, shutdownSignal context.Context) *http.Server {
	mux := http.NewServeMux()

	// Open. The kubelet cannot present a client certificate, and neither
	// discloses anything about the organisation.
	mux.HandleFunc("/healthz", livenessHandler)
	mux.HandleFunc("/readyz", readyHandler(deps, shutdownSignal))

	// Closed. A verified client certificate from this organisation's own CA.
	mux.HandleFunc("/status", requireClientCert(statusHandler(deps)))
	mux.Handle("/metrics", requireClientCert(promhttp.Handler().ServeHTTP))

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// requireClientCert refuses a request that did not present a verified
// certificate.
//
// The listener runs VerifyClientCertIfGiven, so a presented certificate has
// already been checked against the CA and a bad one never reaches here — the
// handshake fails first. What remains is the case this guards: no certificate
// at all, which the listener allows so that the kubelet can probe.
//
// VerifiedChains rather than PeerCertificates. PeerCertificates is populated
// whenever a client sends anything, verified or not; under
// VerifyClientCertIfGiven the two happen to agree, but a later change to
// RequestClientCert would silently turn a check on PeerCertificates into a
// check that any certificate at all is enough.
func requireClientCert(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "this route needs a client certificate", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// statusHandler returns a small JSON document for the console's Health
// page — uptime, server version, current schema version.
//
// Behind requireClientCert. It used to be open, on the argument that it shared
// a port with routes already exposed at the platform layer; see newHealthServer
// for why that argument no longer holds here. The schema version is read fresh
// on each call so the chip reflects the latest applied version without a
// SPA-side cache.
func statusHandler(deps healthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), deps.ProbeTimeout)
		defer cancel()

		var schemaVersion *int64
		var ver int64
		err := deps.Pool.QueryRow(ctx,
			`SELECT MAX(version) FROM atlantis.schema_versions`).Scan(&schemaVersion)
		if err == nil && schemaVersion != nil {
			ver = *schemaVersion
		}

		out := map[string]any{
			"started_at": deps.StartedAt.UTC().Format(time.RFC3339),
			"version":    deps.Version,
		}
		if ver > 0 {
			out["schema_version"] = ver
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

func readyHandler(deps healthDeps, shutdownSignal context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if shutdownSignal.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), deps.ProbeTimeout)
		defer cancel()

		if err := deps.Pool.Ping(ctx); err != nil {
			http.Error(w, "pg: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		// gomemcache has no Ping primitive — a probe Get returns
		// ErrCacheMiss when memcached is reachable and the key doesn't
		// exist; any other error is a network or timeout failure.
		if _, err := deps.MC.Get(ctx, "atl:readyz:probe"); err != nil && !errors.Is(err, runtime.ErrCacheMiss) {
			http.Error(w, "memcached: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if age := time.Since(deps.Worker.LastDrainAt()); age > deps.WorkerMaxStaleness {
			http.Error(w, fmt.Sprintf("outbox worker stale by %s", age), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	}
}
