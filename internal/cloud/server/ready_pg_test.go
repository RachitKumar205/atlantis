package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Readiness reaches the database; liveness does not.
//
// Every sign-in, membership check and session write is a database call, so a
// replica with an unreachable Postgres is not ready. A probe on /healthz or on
// the JWKS route — which proves the signing key loaded and nothing else —
// reports it ready anyway.
//
// The second half holds /healthz the other way. Checking the database there
// restarts a healthy process whenever Postgres hiccups, turning a recoverable
// outage into a crash loop, and a test asserting only readiness passes that.
//
// The pool is closed rather than the database stopped. Postgres here is shared
// with every other test on the machine, and a closed pool gives the handler the
// same answer: a query that cannot be served.
func TestReadyzReachesTheDatabaseAndHealthzDoesNot(t *testing.T) {
	f := newFixture(t)

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d with a working database, want 200: %s",
			rec.Code, rec.Body.String())
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}

	// From here the database is gone as far as this server is concerned.
	f.db.Pool().Close()

	if rec := get("/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d with the database unreachable, want 503. "+
			"A ready replica with no database is one the load balancer sends "+
			"sign-ins that cannot complete. Body: %s", rec.Code, rec.Body.String())
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d with the database unreachable, want 200. "+
			"Liveness must not depend on Postgres: a probe that fails on a "+
			"hiccup restarts a healthy process and makes the outage worse",
			rec.Code)
	}
}
