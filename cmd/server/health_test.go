package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// The health listener's two trust levels.
//
// Open /status and /metrics leave a NetworkPolicy as the only thing keeping
// another tenant's pod away, and that policy is not portable: `ipBlock` covers
// pod traffic under Calico, never covers it under GKE Dataplane V2, and
// excludes nothing on EKS, where pods take VPC addresses. The manifest reads
// the same on all three.
//
// These tests run with no NetworkPolicy. One that proved isolation inside a
// cluster would keep passing where the policy is inert, since the policy would
// be doing the work; here the credential is the only thing standing there.

// healthFixture starts the real listener over TLS and returns its address plus
// the PKI that roots it.
func healthFixture(t *testing.T) (addr string, pki *testpki.PKI) {
	t.Helper()
	pki = testpki.New(t, t.TempDir())

	cert, err := tls.LoadX509KeyPair(pki.CertFile, pki.KeyFile)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	caPEM, err := os.ReadFile(pki.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("no usable certificates in the CA file")
	}

	// The same shape healthTLS builds: verify a certificate if one is given,
	// and allow the request through when none is.
	srv := newHealthServer("", healthDeps{
		ProbeTimeout: time.Second,
		StartedAt:    time.Now(),
		Version:      "test",
	}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.VerifyClientCertIfGiven,
		MinVersion:   tls.VersionTLS12,
	}, context.Background())

	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.TLS = srv.TLSConfig
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "https://"), pki
}

// anonymousClient trusts the server but presents nothing — the kubelet's
// posture.
func anonymousClient(t *testing.T, pki *testpki.PKI) *http.Client {
	t.Helper()
	caPEM, err := os.ReadFile(pki.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

// certClient presents a client certificate from the same authority — the
// console's posture.
func certClient(t *testing.T, pki *testpki.PKI) *http.Client {
	t.Helper()
	certFile, keyFile := pki.ClientCert(t, "atlantis-console")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(pki.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}}
}

func get(t *testing.T, c *http.Client, addr, path string) (int, string) {
	t.Helper()
	resp, err := c.Get("https://" + addr + path) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return resp.StatusCode, string(body)
}

// The kubelet has no certificate and must still be able to probe.
//
// The failure this guards is specific: a liveness probe that cannot
// authenticate makes every pod fail its probe and never become ready, and the
// symptom reads as a broken image rather than as a TLS decision.
func TestTheProbesAnswerWithoutACertificate(t *testing.T) {
	addr, pki := healthFixture(t)
	c := anonymousClient(t, pki)

	if code, _ := get(t, c, addr, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz without a certificate = %d, want 200", code)
	}

	// /readyz is not exercised here. It dereferences the pool, the cache and
	// the outbox worker, so a fixture without them panics rather than answering
	// — which says nothing about the credential gate, and the gate is what this
	// file is about. It is not behind requireClientCert; that is visible in
	// newHealthServer and asserted by the route table below.
	if isGated("/readyz") {
		t.Error("/readyz is behind the certificate gate; the kubelet holds none " +
			"and every pod would fail its probe")
	}
	if isGated("/healthz") {
		t.Error("/healthz is behind the certificate gate")
	}
}

// Without a certificate, the routes that describe a customer's system refuse.
//
// No NetworkPolicy is involved. This is the assertion the plan calls for: the
// credential does the work, so the result does not change with the CNI.
func TestStatusAndMetricsRefuseAnAnonymousCaller(t *testing.T) {
	addr, pki := healthFixture(t)
	c := anonymousClient(t, pki)

	// /metrics over the wire: it needs no database, so a missing gate shows up
	// as a clean 200 rather than as a panic that masks the assertion.
	code, body := get(t, c, addr, "/metrics")
	if code != http.StatusUnauthorized {
		t.Errorf("/metrics without a certificate = %d, want 401", code)
	}
	for _, leak := range []string{"atlantis_", "go_goroutines", "process_"} {
		if strings.Contains(body, leak) {
			t.Errorf("the refusal for /metrics leaks %q: %.200s", leak, body)
		}
	}

	// /status through the route table. Calling it without a database panics
	// whether or not the gate is there, so the wire tells us nothing; what
	// matters is that the gate is in front of it.
	if !isGated("/status") {
		t.Error("/status is not behind the certificate gate — it reports the " +
			"schema and build version of a customer's system")
	}
	if !isGated("/metrics") {
		t.Error("/metrics is not behind the certificate gate")
	}
}

// With a certificate from the organisation's own authority, they answer.
func TestStatusAndMetricsAnswerAVerifiedCaller(t *testing.T) {
	addr, pki := healthFixture(t)
	c := certClient(t, pki)

	// /metrics rather than /status, for the same reason /readyz is skipped
	// above: /status reads the database and this fixture has none. What is
	// under test is whether a verified certificate gets past the gate, and
	// /metrics answers that without any dependency.
	code, body := get(t, c, addr, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics with a certificate = %d", code)
	}
	if !strings.Contains(body, "go_") {
		t.Errorf("/metrics answered without any series: %.200s", body)
	}
}

// A certificate from a different authority does not get in.
//
// This is the cross-tenant case. Organisation B's console holds a perfectly
// valid certificate — from B's authority, which is not A's. The handshake fails
// before any handler runs, which is why the assertion is on the error rather
// than on a status code.
func TestACertificateFromAnotherOrganisationIsRefused(t *testing.T) {
	addr, _ := healthFixture(t)

	// A second, independent authority: a different organisation entirely.
	other := testpki.New(t, t.TempDir())
	certFile, keyFile := other.ClientCert(t, "atlantis-console")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// Trusts the server so the failure is about the client's certificate
	// rather than about the server's.
	firstCA, err := os.ReadFile(other.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = firstCA
	c := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // the client cert is what is under test
			Certificates:       []tls.Certificate{cert},
			MinVersion:         tls.VersionTLS12,
		},
	}}

	if _, err := c.Get("https://" + addr + "/status"); err == nil { //nolint:noctx
		t.Error("a certificate from another organisation's authority was accepted")
	}
}

// isGated reports whether a path is behind requireClientCert.
//
// Asserted directly rather than by calling the route, because two of the four
// routes need a database to answer and their dependencies are not what this
// file is testing. This reads the same wiring newHealthServer sets up: a gated
// handler refuses a request with no TLS state, an open one does not.
func isGated(path string) bool {
	srv := newHealthServer("", healthDeps{ProbeTimeout: time.Second}, nil, context.Background())
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	// No r.TLS at all, which is what requireClientCert refuses on. A handler
	// that runs will panic on the nil pool; one that is gated returns 401
	// before touching it.
	gated := false
	func() {
		defer func() { _ = recover() }()
		srv.Handler.ServeHTTP(rec, r)
	}()
	if rec.Code == http.StatusUnauthorized {
		gated = true
	}
	return gated
}

// /status reports 0 for an empty history and leaves the field out when the
// read failed, so a reader never takes a failed read for version 0.
func TestStatusTellsAnEmptyHistoryFromAFailedRead(t *testing.T) {
	seven := int64(7)
	for _, tc := range []struct {
		name   string
		max    *int64
		err    error
		want   int64
		wantOK bool
	}{
		{"versions", &seven, nil, 7, true},
		{"no versions", nil, nil, 0, true},
		{"failed read", nil, errors.New("timeout"), 0, false},
	} {
		got, ok := reportedSchemaVersion(tc.max, tc.err)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}
