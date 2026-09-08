package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// upstream records what the proxy forwarded.
type upstream struct {
	mu      sync.Mutex
	paths   []string
	bodies  []map[string]any
	hosts   []string
	headers []http.Header
	status  int
}

func (u *upstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.bodies = append(u.bodies, body)
		u.hosts = append(u.hosts, r.Host)
		u.headers = append(u.headers, r.Header.Clone())
		status := u.status
		u.mu.Unlock()

		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Set-Cookie", "ph_session=upstream; Path=/")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}

// proxyServer returns a Server carrying only what handleAnalytics reads, so
// the proxy is testable without a database.
func proxyServer(t *testing.T, host, key string) *Server {
	t.Helper()
	return &Server{
		cfg:          Config{PostHogKey: key, PostHogHost: host},
		log:          slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1})),
		analyticsLim: newLimiterWithMax(analyticsRateLimit),
	}
}

func postAnalytics(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleAnalytics(w, req)
	return w
}

// The three ingestion paths forward and the two that serve JavaScript do not.
func TestAnalyticsForwardsOnlyIngestionPaths(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, path := range []string{"/api/t/e/", "/api/t/i/v0/e/", "/api/t/batch/"} {
		w := postAnalytics(t, s, path, `{"api_key":"x","event":"a"}`)
		if w.Code != http.StatusOK {
			t.Errorf("%s answered %d, want 200", path, w.Code)
		}
	}

	for _, path := range []string{
		"/api/t/static/array.js",
		"/api/t/array/phc_real/config.js",
		"/api/t/flags",
		"/api/t/s/",
		"/api/t/decide",
		"/api/t/",
	} {
		w := postAnalytics(t, s, path, `{"api_key":"x"}`)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, w.Code)
		}
	}

	if got := up.count(); got != 3 {
		t.Errorf("upstream saw %d requests, want 3", got)
	}
}

// Whatever key the caller sends is replaced, so the endpoint can only write
// into this deployment's project.
func TestAnalyticsSubstitutesTheProjectKey(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	w := postAnalytics(t, s, "/api/t/e/", `{"api_key":"phc_someone_else","event":"a.one"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", w.Code)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if got := up.bodies[0]["api_key"]; got != "phc_real" {
		t.Errorf("forwarded api_key %v, want phc_real", got)
	}
	if got := up.bodies[0]["event"]; got != "a.one" {
		t.Errorf("the rest of the body did not survive: %v", up.bodies[0])
	}
}

// PostHog routes on Host and answers 401 when it receives this origin's.
func TestAnalyticsRewritesTheHostHeader(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	postAnalytics(t, s, "/api/t/e/", `{"api_key":"x"}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	if want := hostOf(srv.URL); up.hosts[0] != want {
		t.Errorf("upstream Host %q, want %q", up.hosts[0], want)
	}
}

// Credentials the browser attached do not reach a third party, and the
// server's own address resolution is what upstream sees.
func TestAnalyticsStripsCredentialsAndSetsForwardedFor(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	req := httptest.NewRequest(http.MethodPost, "/api/t/e/", strings.NewReader(`{"api_key":"x"}`))
	req.RemoteAddr = "198.51.100.7:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "atl_cloud_session=secret")
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Referer", "https://platform.example/private")
	req.Header.Set("User-Agent", "spoofed/1.0")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	s.handleAnalytics(httptest.NewRecorder(), req)

	up.mu.Lock()
	defer up.mu.Unlock()
	h := up.headers[0]
	for _, name := range []string{"Cookie", "Authorization", "Referer"} {
		if v := h.Get(name); v != "" {
			t.Errorf("%s crossed to the upstream: %q", name, v)
		}
	}
	if v := h.Get("User-Agent"); strings.Contains(v, "spoofed") {
		t.Errorf("the caller's User-Agent crossed: %q", v)
	}
	// TrustProxy is off in this config, so the header the caller set is
	// ignored and the socket address is used.
	if v := h.Get("X-Forwarded-For"); v != "198.51.100.7" {
		t.Errorf("X-Forwarded-For %q, want the resolved address", v)
	}
}

// The query string is dropped, so nothing the caller appends reaches PostHog.
func TestAnalyticsDropsTheQueryString(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	postAnalytics(t, s, "/api/t/e/?ip=1&compression=gzip-js", `{"api_key":"x"}`)

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.paths[0] != "/e" {
		t.Errorf("upstream path %q, want /e", up.paths[0])
	}
}

// Nothing upstream sets a cookie on this origin.
func TestAnalyticsDoesNotForwardSetCookie(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	w := postAnalytics(t, s, "/api/t/e/", `{"api_key":"x"}`)
	if v := w.Header().Get("Set-Cookie"); v != "" {
		t.Errorf("Set-Cookie crossed back: %q", v)
	}
}

func TestAnalyticsRejectsNonPost(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		req := httptest.NewRequest(method, "/api/t/e/", nil)
		req.RemoteAddr = "198.51.100.7:1234"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleAnalytics(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405", method, w.Code)
		}
	}
	if up.count() != 0 {
		t.Error("a non-POST reached the upstream")
	}
}

func TestAnalyticsRejectsAnOversizedBody(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	big := `{"api_key":"x","pad":"` + strings.Repeat("a", analyticsMaxBody) + `"}`
	w := postAnalytics(t, s, "/api/t/e/", big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("answered %d, want 413", w.Code)
	}
	if up.count() != 0 {
		t.Error("an oversized body reached the upstream")
	}
}

func TestAnalyticsRejectsAnOversizedBatch(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	events := make([]string, analyticsMaxBatch+1)
	for i := range events {
		events[i] = `{"event":"a"}`
	}
	body := `{"api_key":"x","batch":[` + strings.Join(events, ",") + `]}`

	w := postAnalytics(t, s, "/api/t/batch/", body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("answered %d, want 413", w.Code)
	}
	if up.count() != 0 {
		t.Error("an oversized batch reached the upstream")
	}
}

// A body whose key cannot be substituted is refused, because substitution is
// what stops the endpoint being a relay.
func TestAnalyticsRejectsANonJSONBody(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	w := postAnalytics(t, s, "/api/t/e/", "data=compressed-bytes")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("answered %d, want 415", w.Code)
	}
	if up.count() != 0 {
		t.Error("an unparseable body reached the upstream")
	}
}

// With no key configured the route does not exist, which is every development
// and CI build.
func TestAnalyticsIsAbsentWithoutAKey(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "")

	w := postAnalytics(t, s, "/api/t/e/", `{"api_key":"x"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("answered %d, want 404", w.Code)
	}
	if up.count() != 0 {
		t.Error("an event was forwarded with no key configured")
	}
}

// An upstream failure is reported as a gateway error and nothing is retried.
func TestAnalyticsReportsAnUpstreamFailure(t *testing.T) {
	s := proxyServer(t, "http://127.0.0.1:1", "phc_real")

	w := postAnalytics(t, s, "/api/t/e/", `{"api_key":"x"}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("answered %d, want 502", w.Code)
	}
}

// Percent-encoded traversal reaches the handler unchanged.
//
// Measured against net/http's ServeMux: it redirects a literal "/api/t/../x"
// and collapses "//", but "%2e%2e" is decoded after that and arrives whole.
// The path allowlist is an exact-match map, so the decoded segments match
// nothing and no request is built.
func TestAnalyticsRefusesEncodedTraversal(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, path := range []string{
		"/api/t/../../admin",
		"/api/t/../../../etc/passwd",
		"/api/t/e/%00",
		"/api/t/e/../static/array.js",
		"/api/t/E/",
	} {
		w := postAnalytics(t, s, path, `{"api_key":"x"}`)
		if w.Code != http.StatusNotFound {
			t.Errorf("%q answered %d, want 404", path, w.Code)
		}
	}
	if up.count() != 0 {
		t.Errorf("%d traversal attempts reached the upstream", up.count())
	}
}

// Redundant slashes around an allowlisted segment resolve to the same
// endpoint. The mux collapses them before the handler in a served request;
// this records what the handler does with them on its own.
func TestAnalyticsToleratesRedundantSlashes(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	if w := postAnalytics(t, s, "/api/t/e//", `{"api_key":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", w.Code)
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.paths[0] != "/e" {
		t.Errorf("upstream path %q, want /e", up.paths[0])
	}
}

// The route as registered delivers what the handler expects.
//
// The tests above call the handler directly, so this is the one that reads the
// pattern through a mux and shows the router does not decode away the segments
// the allowlist is checking.
func TestAnalyticsRouteDeliversTheRestOfThePath(t *testing.T) {
	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+analyticsPathPrefix+"{rest...}", func(_ http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
	})

	for path, want := range map[string]string{
		"/api/t/e/":              "/api/t/e/",
		"/api/t/batch/":          "/api/t/batch/",
		"/api/t/%2e%2e/%2e%2e/x": "/api/t/../../x",
	} {
		seen = ""
		req := httptest.NewRequest(http.MethodPost, path, nil)
		mux.ServeHTTP(httptest.NewRecorder(), req)
		if seen != want {
			t.Errorf("%q reached the handler as %q, want %q", path, seen, want)
		}
	}
}

// PostHog reads a token from $token, token and properties.token ahead of
// api_key, so substituting api_key alone would leave three ways to name
// somebody else's project.
func TestAnalyticsRemovesEveryTokenField(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, body := range []string{
		`{"$token":"phc_victim","event":"a"}`,
		`{"token":"phc_victim","event":"a"}`,
		`{"api_key":"phc_victim","event":"a"}`,
		`{"$token":"phc_victim","token":"phc_victim","api_key":"phc_victim","event":"a"}`,
		`{"properties":{"token":"phc_victim","x":1},"event":"a"}`,
	} {
		w := postAnalytics(t, s, "/api/t/e/", body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d, want 200", body, w.Code)
		}
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	for i, got := range up.bodies {
		if got["api_key"] != "phc_real" {
			t.Errorf("body %d forwarded api_key %v, want phc_real", i, got["api_key"])
		}
		for _, field := range []string{"$token", "token"} {
			if v, ok := got[field]; ok {
				t.Errorf("body %d still carries %s=%v", i, field, v)
			}
		}
		if props, ok := got["properties"].(map[string]any); ok {
			if v, ok := props["token"]; ok {
				t.Errorf("body %d still carries properties.token=%v", i, v)
			}
			if props["x"] == nil {
				t.Errorf("body %d lost the other properties: %v", i, props)
			}
		}
	}
}

// A four-byte body used to panic the handler: a JSON null decodes into a nil
// map without error, and the key substitution then assigned to it.
func TestAnalyticsRefusesABodyThatIsNotAnObject(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, body := range []string{`null`, `[]`, `"a"`, `7`, `true`} {
		w := postAnalytics(t, s, "/api/t/e/", body)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s answered %d, want 415", body, w.Code)
		}
	}
	if up.count() != 0 {
		t.Error("a body that is not an object reached the upstream")
	}
}

// Without a JSON content type the request would be a CORS simple request, and
// any page on the internet could write events into this project.
func TestAnalyticsRequiresAJSONContentType(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		req := httptest.NewRequest(http.MethodPost, "/api/t/e/", strings.NewReader(`{"event":"a"}`))
		req.RemoteAddr = "198.51.100.7:1234"
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		s.handleAnalytics(w, req)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q answered %d, want 415", ct, w.Code)
		}
	}
	if up.count() != 0 {
		t.Error("a simple-request content type reached the upstream")
	}
}

// An Origin from somewhere else is refused; this deployment's own is not.
func TestAnalyticsChecksTheOriginWhenPresent(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")
	s.cfg.PublicURL = "https://platform.example.dev"

	post := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/t/e/", strings.NewReader(`{"event":"a"}`))
		req.RemoteAddr = "198.51.100.7:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.handleAnalytics(w, req)
		return w.Code
	}

	if got := post("https://platform.example.dev"); got != http.StatusOK {
		t.Errorf("this deployment's own origin answered %d, want 200", got)
	}
	for _, origin := range []string{"https://evil.example", "null", "http://platform.example.dev.evil"} {
		if got := post(origin); got != http.StatusForbidden {
			t.Errorf("origin %q answered %d, want 403", origin, got)
		}
	}
}

// The limit is per address, so one exhausted caller does not refuse another.
func TestAnalyticsRateLimitsPerAddressNotGlobally(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")

	post := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/t/e/", strings.NewReader(`{"event":"a"}`))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.handleAnalytics(w, req)
		return w.Code
	}

	var exhausted bool
	for i := 0; i < analyticsRateLimit+5; i++ {
		if post("198.51.100.7:1234") == http.StatusTooManyRequests {
			exhausted = true
			break
		}
	}
	if !exhausted {
		t.Fatal("one address was never limited")
	}
	if got := post("203.0.113.4:9999"); got != http.StatusOK {
		t.Errorf("a second address answered %d, want 200", got)
	}
}

// Google's load balancer appends to X-Forwarded-For, so the leftmost element
// is whatever the caller sent. Reading it would let one caller reset the
// limiter on every request by changing a string.
func TestForwardedClientIPReadsTheTerminatorsValue(t *testing.T) {
	for _, tc := range []struct {
		xff  string
		want string
	}{
		{"198.51.100.7,35.191.0.1", "198.51.100.7"},
		{"attacker-chose-this,198.51.100.7,35.191.0.1", "198.51.100.7"},
		{"203.0.113.9, 198.51.100.7, 35.191.0.1", "198.51.100.7"},
		{"198.51.100.7", "198.51.100.7"},
		{"not-an-address,also-not", ""},
		{"", ""},
		{strings.Repeat("a", 9000), ""},
	} {
		if got := forwardedClientIP(tc.xff); got != tc.want {
			t.Errorf("forwardedClientIP(%.40q) = %q, want %q", tc.xff, got, tc.want)
		}
	}
}

// With the proxy trusted, the limiter keys on the address the terminator
// observed rather than one the caller supplied.
func TestAnalyticsKeysOnTheTerminatorsAddress(t *testing.T) {
	up := &upstream{}
	srv := up.server(t)
	s := proxyServer(t, srv.URL, "phc_real")
	s.cfg.TrustProxy = true

	req := httptest.NewRequest(http.MethodPost, "/api/t/e/", strings.NewReader(`{"event":"a"}`))
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "spoofed,198.51.100.7,35.191.0.1")
	s.handleAnalytics(httptest.NewRecorder(), req)

	up.mu.Lock()
	defer up.mu.Unlock()
	if v := up.headers[0].Get("X-Forwarded-For"); v != "198.51.100.7" {
		t.Errorf("forwarded %q, want the terminator's value", v)
	}
}

func TestHostOfKeepsThePort(t *testing.T) {
	for in, want := range map[string]string{
		"https://us.i.posthog.com":      "us.i.posthog.com",
		"https://us.i.posthog.com/":     "us.i.posthog.com",
		"http://127.0.0.1:54321":        "127.0.0.1:54321",
		"http://127.0.0.1:54321/batch/": "127.0.0.1:54321",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
