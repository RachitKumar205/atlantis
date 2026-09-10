package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

// The header is read only where the deployment says something it controls
// terminates in front.
func TestClientIPHonoursTrustProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/t/e/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "spoofed,198.51.100.7,35.191.0.1")

	untrusted := &Server{cfg: Config{}}
	if got := untrusted.clientIP(req); got != "10.0.0.1" {
		t.Errorf("untrusted resolved %q, want the socket address", got)
	}

	trusted := &Server{cfg: Config{TrustProxy: true}}
	if got := trusted.clientIP(req); got != "198.51.100.7" {
		t.Errorf("trusted resolved %q, want the terminator's value", got)
	}
}

// A trusted deployment whose terminator sent nothing usable falls back to the
// socket address rather than counting every caller as one key.
func TestClientIPFallsBackWhenTheHeaderIsUnusable(t *testing.T) {
	trusted := &Server{cfg: Config{TrustProxy: true}}
	for _, xff := range []string{"", "not-an-address", "also-not,still-not"} {
		req := httptest.NewRequest(http.MethodPost, "/api/t/e/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		if got := trusted.clientIP(req); got != "10.0.0.1" {
			t.Errorf("X-Forwarded-For %q resolved %q, want the socket address", xff, got)
		}
	}
}
