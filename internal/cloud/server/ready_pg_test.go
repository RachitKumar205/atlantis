package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Readiness reaches the database; liveness does not.
//
// # Why both halves are asserted
//
// Cloud had no /readyz at all, so its Deployment probed either /healthz — a
// bare 200 — or the JWKS route, which proves the signing key loaded and nothing
// more. Under both, a replica with an unreachable Postgres reported itself
// ready and was sent traffic it could not serve: every sign-in, membership
// check and session write is a database call.
//
// So the first half is the fix. The second half is what stops the fix becoming
// a different fault: making /healthz check the database too would restart a
// healthy process whenever Postgres hiccupped, converting a recoverable outage
// into a crash loop. Liveness answering while the database is down is the
// correct behaviour, and a test that only checked readiness would be satisfied
// by a change that broke it.
//
// The pool is closed rather than the database stopped. Stopping Postgres is
// what a deployment would actually experience, but it is shared with every
// other test and process on this machine; closing the pool produces the same
// answer from the handler's point of view — a query that cannot be served —
// without taking the environment down to prove it.
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
