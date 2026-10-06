package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Serving the sign-in application, and the routes it must not swallow.

// appFS is a stand-in for a built web/cloud.
func appFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html>CLOUD")},
		"assets/index-abc123.js": {Data: []byte("console.log(1)")},
	}
}

// do sends a request with any method.
//
// The existing helpers are per-method; the 405-versus-404 property needs the
// same path under two of them.
func (f *fixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = f.nextIP() + ":1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

// The application is served, and client routes reach it.
func TestTheSignInAppIsServed(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = appFS()

	for _, path := range []string{"/", "/signin", "/signup", "/organisations"} {
		rec := f.get(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "CLOUD") {
			t.Errorf("%s: body = %q", path, rec.Body.String())
		}
	}
}

// Without the embedspa tag there is no app, and the answer says what to do.
//
// A blank page or a bare 404 would leave somebody guessing at a build flag.
func TestAnUnbuiltAppNamesTheCommandThatBuildsIt(t *testing.T) {
	f := newFixture(t) // nil spaFS, as every untagged build has

	rec := f.get(t, "/signin")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make build-cloud-spa") {
		t.Errorf("the answer does not name the fix: %q", rec.Body.String())
	}
}

// Every route that existed before the catch-all still wins over it.
//
// `GET /` matches every GET path, so this is the property that keeps the whole
// API reachable. Measured rather than reasoned about.
func TestExistingRoutesWinOverTheCatchAll(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = appFS()

	// Each of these must NOT be answered by the application.
	for _, path := range []string{
		"/.well-known/jwks.json",
		"/healthz",
		"/api/auth/config",
		"/api/auth/pending",
	} {
		rec := f.get(t, path)
		if strings.Contains(rec.Body.String(), "CLOUD") {
			t.Errorf("%s was answered by the SPA: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

// An unmatched API path is a JSON 404, not the application.
//
// Behind the catch-all it would otherwise be 200 with index.html, which reaches
// a client as a JSON parse error naming nothing relevant.
//
// POST as well as GET: registering only `GET /api/` leaves POST matching the
// path but not the method, which net/http answers with 405 and text/plain.
func TestUnmatchedAPIPathsAreJSONNotFound(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = appFS()

	for _, m := range []string{http.MethodGet, http.MethodPost} {
		rec := f.do(t, m, "/api/nope", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /api/nope: status %d, want 404", m, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s /api/nope: content-type %q, want JSON", m, ct)
		}
		if strings.Contains(rec.Body.String(), "CLOUD") {
			t.Errorf("%s /api/nope was answered by the SPA", m)
		}
	}
}

// An unconfigured provider is a 404, not the application.
//
// OAuth routes are registered only for providers this deployment has
// credentials for, so /auth/google was a clean 404 before the catch-all existed
// and would silently become 200 index.html after it.
func TestAnUnconfiguredProviderIsNotTheApp(t *testing.T) {
	f := newFixtureWithoutProviders(t)
	f.srv.spaFS = appFS()

	for _, path := range []string{"/auth/google", "/auth/github", "/auth/google/callback"} {
		rec := f.get(t, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "CLOUD") {
			t.Errorf("%s was answered by the SPA", path)
		}
	}
}

// A configured provider still wins over the fallback.
//
// A literal path segment beats a wildcard, so registering `GET /auth/{provider}`
// as a 404 must not shadow `GET /auth/github`.
func TestAConfiguredProviderStillWorks(t *testing.T) {
	f := newFixture(t) // github and google are configured here
	f.srv.spaFS = appFS()

	rec := f.get(t, "/auth/github")
	if rec.Code != http.StatusFound && rec.Code != http.StatusSeeOther {
		t.Fatalf("a configured provider did not redirect: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "github.com") {
		t.Errorf("Location = %q, want GitHub", loc)
	}
}

// Every route except the app keeps `default-src 'none'`.
//
// Asserted as the EXACT policy, not as a substring. `default-src 'none'` is a
// prefix of the relaxed policy W7 gives the card pages, so a
// contains-check passes a regression — which is exactly what the existing
// assertion in auth_pg_test.go does.
func TestNonAppRoutesKeepTheStrictPolicy(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = appFS()

	for _, path := range []string{"/healthz", "/api/auth/config", "/api/nope", "/api/t/e/"} {
		got := f.get(t, path).Header().Get("Content-Security-Policy")
		if got != strictCSP {
			t.Errorf("%s:\n got  %q\n want %q", path, got, strictCSP)
		}
	}
}

// The analytics endpoint moves no policy: the page posts to this origin and
// the server forwards, so `connect-src 'self'` already covers ingestion.
func TestTheAppPolicyNamesNoAnalyticsHost(t *testing.T) {
	for _, banned := range []string{
		"posthog",
		"blob:",      // the replay recorder's compression worker
		"worker-src", // which it would need
	} {
		if strings.Contains(spaCSP, banned) {
			t.Errorf("the sign-in app's policy carries %s:\n%s", banned, spaCSP)
		}
		if strings.Contains(strictCSP, banned) {
			t.Errorf("the strict policy carries %s:\n%s", banned, strictCSP)
		}
	}
}

// The application gets the wider policy, and only the application.
func TestTheAppGetsTheWiderPolicy(t *testing.T) {
	f := newFixture(t)
	f.srv.spaFS = appFS()

	got := f.get(t, "/signin").Header().Get("Content-Security-Policy")
	if got != spaCSP {
		t.Errorf("\n got  %q\n want %q", got, spaCSP)
	}
}

// The wider policy is still narrow where it counts.
//
// Asserted by name so that adding any of these is a deliberate edit to a test
// rather than a line in a commit nobody reads. `unsafe-inline` for styles is
// the one under review at the end of W4 — if it is added, it is added here too,
// with the reason.
func TestTheAppPolicyAllowsNothingDangerous(t *testing.T) {
	for _, banned := range []string{
		"'unsafe-inline'",
		"'unsafe-eval'",
		"data:",
		"https://", // no CDN, no Google Fonts: this origin fetches nothing
		"'unsafe-hashes'",
	} {
		if strings.Contains(spaCSP, banned) {
			t.Errorf("the sign-in app's policy allows %s:\n%s", banned, spaCSP)
		}
	}
}

// A route added later inherits the strict policy rather than the loose one.
//
// This is the fail-closed property. securityHeaders sets strictCSP before the
// mux runs, so forgetting to think about CSP produces the safe answer.
func TestANewRouteInheritsTheStrictPolicy(t *testing.T) {
	f := newFixture(t)
	f.srv.mux.HandleFunc("GET /freshly-added", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if got := f.get(t, "/freshly-added").Header().Get("Content-Security-Policy"); got != strictCSP {
		t.Errorf("a route that ignored CSP got %q, want the strict policy", got)
	}
}
