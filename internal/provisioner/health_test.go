package provisioner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthzAnswersWhileTheQueueIsUnreachable(t *testing.T) {
	srv := NewHealthServer(":0", func(context.Context) error {
		return errors.New("connection refused")
	}, context.Background())

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// Liveness is "this process is up", not "this process can work". Failing it
	// on a database outage asks Kubernetes to restart every provisioner every
	// time Postgres hiccups, which fixes nothing and loses whatever was in
	// flight.
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 even when the queue is unreachable", rec.Code)
	}
}

func TestReadyzFailsWhenTheQueueIsUnreachable(t *testing.T) {
	srv := NewHealthServer(":0", func(context.Context) error {
		return errors.New("connection refused")
	}, context.Background())

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503: a provisioner that cannot read its queue "+
			"provisions nobody", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("/readyz does not say why: %q", rec.Body.String())
	}
}

func TestReadyzFailsOnceShutdownHasBegun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewHealthServer(":0", func(context.Context) error { return nil }, ctx)

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d before shutdown, want 200", rec.Code)
	}

	cancel()

	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d after shutdown began, want 503 so an orchestrator "+
			"stops routing before the process goes away", rec.Code)
	}
}

// /metrics carries this package's own series, not an empty page.
//
// The trap this guards: promhttp.Handler() serves the default registry, and
// nothing in this binary's import graph declares an atlantis collector —
// neither internal/cloud/store nor internal/cloud/provision pulls in prometheus
// at all. Mounted without the collectors in metrics.go, /metrics answers 200
// with go_* and process_* and nothing about provisioning, which looks wired and
// reports nothing. Asserting "200 and non-empty" would pass against exactly
// that, so this asserts a named family instead.
// The health listener must not serve metrics.
//
// It is the listener the kubelet probes, so it binds every interface and cannot
// require a credential. Anything on it is readable by every pod in the cluster:
// tenant namespaces restrict ingress, not egress, so a tenant workload can open
// a connection to any address it can reach.
//
// This is the assertion, not a tidiness check. Moving /metrics to its own
// address is undone by one line, and the undo looks like a merge conflict
// resolved the obvious way.
func TestTheHealthListenerDoesNotServeMetrics(t *testing.T) {
	srv := NewHealthServer(":0", nil, context.Background())

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code == http.StatusOK {
		t.Errorf("/metrics answered 200 on the health listener, which is open to "+
			"every pod in the cluster; it belongs on MetricsAddr. Body: %.120s",
			rec.Body.String())
	}

	// The probes stay, and are checked here rather than trusted: a split that
	// took the probes with it would leave a pod Kubernetes cannot restart.
	for _, path := range []string{"/healthz", "/readyz"} {
		r := httptest.NewRecorder()
		srv.Handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusOK {
			t.Errorf("%s = %d on the health listener, want 200", path, r.Code)
		}
	}
}

func TestMetricsCarriesProvisioningSeries(t *testing.T) {
	srv := NewMetricsServer(":0")

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}

	// A plain Counter reports from the moment it is registered. The CounterVecs
	// only appear once a label combination has been observed, so this is the
	// one that proves the collectors reached the default registry at all.
	if !strings.Contains(string(body), "atlantis_provisioning_claim_failures_total") {
		t.Error("/metrics carries no atlantis_provisioning_* series: this binary " +
			"would be scraped and report nothing about provisioning")
	}
}
