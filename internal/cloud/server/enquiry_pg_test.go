package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The marketing site's form, against a real database.

const testSite = "https://site.test"

func newEnquiryFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, func(c *Config) { c.SiteOrigin = testSite })
}

// enquire posts body to /api/enquiries from ip, with origin as the Origin
// header unless it is empty.
func (f *fixture) enquire(t *testing.T, ip, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/enquiries", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

func enquiryBody(t *testing.T, email, message, website string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"email": email, "message": message, "website": website})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) enquiryCount(t *testing.T) int {
	t.Helper()
	rows, err := f.db.Enquiries(context.Background())
	if err != nil {
		t.Fatalf("list enquiries: %v", err)
	}
	return len(rows)
}

func TestEnquiryIsStored(t *testing.T) {
	f := newEnquiryFixture(t)

	rec := f.enquire(t, f.nextIP(), testSite,
		enquiryBody(t, " Ada@Example.COM ", "  We run three agents.\r\nOne writes rows.  ", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("enquiry: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testSite {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, testSite)
	}

	rows, err := f.db.Enquiries(context.Background())
	if err != nil {
		t.Fatalf("list enquiries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d enquiries, want 1", len(rows))
	}
	if rows[0].Email != "ada@example.com" {
		t.Errorf("email = %q, want it folded to ada@example.com", rows[0].Email)
	}
	if want := "We run three agents.\nOne writes rows."; rows[0].Message != want {
		t.Errorf("message = %q, want %q", rows[0].Message, want)
	}
}

// Without Origin, or from another site, nothing is stored and nothing in the
// answer lets a page read it.
func TestEnquiryRefusesOtherOrigins(t *testing.T) {
	f := newEnquiryFixture(t)

	for _, origin := range []string{"", "https://elsewhere.test", "null", testSite + ".evil.test"} {
		rec := f.enquire(t, f.nextIP(), origin, enquiryBody(t, "ada@example.com", "hello", ""))
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: %d, want 403", origin, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Origin %q: Access-Control-Allow-Origin = %q, want none", origin, got)
		}
	}
	if n := f.enquiryCount(t); n != 0 {
		t.Fatalf("stored %d enquiries from refused origins", n)
	}
}

// Each refusal the site words for the visitor has to be readable by the site,
// so it carries the CORS header too.
func TestEnquiryRefusalsCarryCORS(t *testing.T) {
	f := newEnquiryFixture(t)

	long := strings.Repeat("a", 4001)
	// 3998 + CRLF + 1 is 4001 characters as sent and 4000 once folded.
	foldsToLimit := strings.Repeat("a", 3998) + "\r\nb"

	cases := []struct {
		name, body string
		want       int
	}{
		{"malformed", `{"email":`, http.StatusBadRequest},
		{"unknown field", `{"email":"ada@example.com","message":"hi","name":"Ada"}`, http.StatusBadRequest},
		{"bad email", enquiryBody(t, "not an address", "hi", ""), http.StatusBadRequest},
		{"NUL in email", enquiryBody(t, "ada\x00@example.com", "hi", ""), http.StatusBadRequest},
		{"empty message", enquiryBody(t, "ada@example.com", "", ""), http.StatusBadRequest},
		{"blank message", enquiryBody(t, "ada@example.com", " \r\n\t ", ""), http.StatusBadRequest},
		{"long message", enquiryBody(t, "ada@example.com", long, ""), http.StatusBadRequest},
		{"NUL in message", enquiryBody(t, "ada@example.com", "a\x00b", ""), http.StatusBadRequest},
		{"CRLF folds under the limit", enquiryBody(t, "ada@example.com", foldsToLimit, ""), http.StatusOK},
	}
	for _, c := range cases {
		rec := f.enquire(t, f.nextIP(), testSite, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testSite {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want %q", c.name, got, testSite)
		}
	}
	if n := f.enquiryCount(t); n != 1 {
		t.Fatalf("stored %d enquiries, want only the one at the limit", n)
	}
}

// A filled honeypot is answered as a person is, and nothing is stored.
func TestEnquiryHoneypotStoresNothing(t *testing.T) {
	f := newEnquiryFixture(t)

	rec := f.enquire(t, f.nextIP(), testSite,
		enquiryBody(t, "bot@example.com", "buy now", "https://spam.test"))
	if rec.Code != http.StatusOK {
		t.Fatalf("honeypot: %d %s, want the 200 a person gets", rec.Code, rec.Body.String())
	}
	if n := f.enquiryCount(t); n != 0 {
		t.Fatalf("stored %d enquiries with the honeypot filled", n)
	}
}

// The sixth enquiry in a minute from one address is refused, readably, and the
// address can still sign in: the two routes do not share a budget.
func TestEnquiryRateLimitIsSeparateFromSignIn(t *testing.T) {
	f := newEnquiryFixture(t)
	const ip = "198.51.100.7"

	for i := range limiterMax {
		if rec := f.enquire(t, ip, testSite, enquiryBody(t, "ada@example.com", "hello", "")); rec.Code != http.StatusOK {
			t.Fatalf("enquiry %d: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := f.enquire(t, ip, testSite, enquiryBody(t, "ada@example.com", "hello", ""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("enquiry %d: %d, want 429", limiterMax+1, rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testSite {
		t.Errorf("429: Access-Control-Allow-Origin = %q, want %q", got, testSite)
	}

	login := f.postFrom(t, ip, "/api/auth/login", `{"email":"ada@example.com","password":"wrong password"}`)
	if login.Code == http.StatusTooManyRequests {
		t.Fatal("sign-in was rate limited by enquiries from the same address")
	}
}

// At the daily cap the form answers 503, readably, and stores nothing.
func TestEnquiryDailyCapAnswers503(t *testing.T) {
	f := newEnquiryFixture(t)

	if _, err := f.db.Pool().Exec(context.Background(), `
		INSERT INTO cloud.enquiries (email, message)
		SELECT 'ada@example.com', 'hello' FROM generate_series(1, $1::int)`,
		store.EnquiryDailyCap); err != nil {
		t.Fatalf("fill to the cap: %v", err)
	}

	rec := f.enquire(t, f.nextIP(), testSite, enquiryBody(t, "ada@example.com", "one too many", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("enquiry over the cap: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testSite {
		t.Errorf("503: Access-Control-Allow-Origin = %q, want %q", got, testSite)
	}
	if n := f.enquiryCount(t); n != store.EnquiryDailyCap {
		t.Fatalf("stored %d enquiries, want the %d at the cap", n, store.EnquiryDailyCap)
	}
}

func TestEnquiryPreflightAdmitsOnlyTheSite(t *testing.T) {
	f := newEnquiryFixture(t)

	preflight := func(origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodOptions, "/api/enquiries", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "content-type")
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, r)
		return rec
	}

	rec := preflight(testSite)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testSite {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, testSite)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got != "POST" {
		t.Errorf("Access-Control-Allow-Methods = %q, want POST", got)
	}
	if got := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")); got != "content-type" {
		t.Errorf("Access-Control-Allow-Headers = %q, want Content-Type", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want none: the route reads no cookie", got)
	}

	if got := preflight("https://elsewhere.test").Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("another origin's preflight: Access-Control-Allow-Origin = %q, want none", got)
	}
}

// With no site origin configured the routes do not exist: POST falls to the
// /api/ catch-all, and OPTIONS matches no method on the path.
func TestEnquiryRoutesAbsentWithoutSiteOrigin(t *testing.T) {
	f := newFixture(t)

	rec := f.enquire(t, f.nextIP(), testSite, enquiryBody(t, "ada@example.com", "hello", ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST: %d, want 404", rec.Code)
	}

	r := httptest.NewRequest(http.MethodOptions, "/api/enquiries", nil)
	r.Header.Set("Origin", testSite)
	opts := httptest.NewRecorder()
	f.srv.ServeHTTP(opts, r)
	if opts.Code != http.StatusMethodNotAllowed {
		t.Errorf("OPTIONS: %d, want 405", opts.Code)
	}
	for _, h := range []*httptest.ResponseRecorder{rec, opts} {
		if got := h.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
		}
	}
	if n := f.enquiryCount(t); n != 0 {
		t.Fatalf("stored %d enquiries with the feature off", n)
	}
}
