package console

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// exhaust reports how many requests one address made before it was refused,
// and the Retry-After it was given. It gives up at ceiling.
func exhaust(t *testing.T, l *loginLimiter, ip string, ceiling int) (allowed, retry int) {
	t.Helper()
	for i := 0; i < ceiling; i++ {
		ok, after := l.allow(ip)
		if !ok {
			return i, after
		}
	}
	t.Fatalf("%s was allowed %d requests with no refusal", ip, ceiling)
	return 0, 0
}

// The two budgets are different numbers, and each limiter carries its own.
//
// The budget was a package constant until the ingestion route needed a second
// one. A limiter that ignored its own field would answer with the assertion
// budget for browser analytics, which is 10 events a minute.
func TestEachLimiterCarriesItsOwnBudget(t *testing.T) {
	if allowed, _ := exhaust(t, newLoginLimiter(), "198.51.100.7", 1000); allowed != loginLimiterMax {
		t.Errorf("the assertion routes allowed %d, want %d", allowed, loginLimiterMax)
	}
	events := newLoginLimiterWithMax(analytics.ProxyRateLimit, analytics.ProxyMaxAddresses)
	if allowed, _ := exhaust(t, events, "198.51.100.7", 1000); allowed != analytics.ProxyRateLimit {
		t.Errorf("the ingestion route allowed %d, want %d", allowed, analytics.ProxyRateLimit)
	}
}

// One exhausted caller does not refuse another. Both routes are keyed on an
// address, and a shared limit would let one browser lock out every other.
func TestTheBudgetIsPerAddress(t *testing.T) {
	for _, l := range []*loginLimiter{
		newLoginLimiter(),
		newLoginLimiterWithMax(analytics.ProxyRateLimit, analytics.ProxyMaxAddresses),
	} {
		exhaust(t, l, "198.51.100.7", 1000)
		if ok, _ := l.allow("203.0.113.4"); !ok {
			t.Error("a second address was refused after the first was exhausted")
		}
	}
}

// A refusal says when to come back, and the answer is a number a client can
// wait for.
func TestARefusalReportsAUsableRetry(t *testing.T) {
	_, retry := exhaust(t, newLoginLimiter(), "198.51.100.7", 1000)
	if retry < 1 || retry > int(loginLimiterWindow.Seconds()) {
		t.Errorf("Retry-After is %d, want 1 to %d", retry, int(loginLimiterWindow.Seconds()))
	}
}

// The header is read only where the deployment says something it controls
// terminates in front, and the client is second from the end because Google's
// load balancer appends to whatever arrived.
func TestClientIPHonoursTrustProxy(t *testing.T) {
	req := func(xff string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/t/batch/", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	if got := clientIP(req("spoofed,198.51.100.7,35.191.0.1")); got != "10.0.0.1" {
		t.Errorf("untrusted resolved %q, want the socket address", got)
	}

	t.Setenv("CONSOLE_TRUST_PROXY", "true")
	if got := clientIP(req("spoofed,198.51.100.7,35.191.0.1")); got != "198.51.100.7" {
		t.Errorf("trusted resolved %q, want the terminator's value", got)
	}

	// Anything that does not parse as an address falls back to the socket,
	// so a key stays bounded in length and count.
	for _, xff := range []string{"", "not-an-address", "also-not,still-not"} {
		if got := clientIP(req(xff)); got != "10.0.0.1" {
			t.Errorf("X-Forwarded-For %q resolved %q, want the socket address", xff, got)
		}
	}
}

// The address cap bounds the memory an unauthenticated route can make this
// process hold. A new address arriving at the cap is refused; one already
// tracked keeps its budget.
func TestTheAddressCapRefusesNewAddresses(t *testing.T) {
	l := newLoginLimiterWithMax(loginLimiterMax, 2)
	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("%s was refused below the cap", ip)
		}
	}
	if ok, _ := l.allow("203.0.113.9"); ok {
		t.Error("an address past the cap was tracked, so the ceiling is not a ceiling")
	}
	if ok, _ := l.allow("198.51.100.1"); !ok {
		t.Error("an address already tracked was refused at the cap")
	}
}
