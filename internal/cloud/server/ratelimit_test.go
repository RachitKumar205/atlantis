package server

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// Each limiter carries its own budget. The auth routes, the CLI poll and the
// ingestion route are three different numbers against one type.
func TestEachLimiterCarriesItsOwnBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    *limiter
		want int
	}{
		{"auth", newLimiter(), limiterMax},
		{"cli poll", newLimiterWithMax(cliPollLimit, limiterMaxIPs), cliPollLimit},
		{"ingestion", newLimiterWithMax(analytics.ProxyRateLimit, analytics.ProxyMaxAddresses), analytics.ProxyRateLimit},
	} {
		allowed := 0
		for i := 0; i < tc.want+10; i++ {
			ok, _ := tc.l.allow("198.51.100.7")
			if !ok {
				break
			}
			allowed++
		}
		if allowed != tc.want {
			t.Errorf("%s allowed %d, want %d", tc.name, allowed, tc.want)
		}
	}
}

// One exhausted caller does not refuse another.
func TestTheBudgetIsPerKey(t *testing.T) {
	l := newLimiterWithMax(analytics.ProxyRateLimit, analytics.ProxyMaxAddresses)
	for i := 0; i < analytics.ProxyRateLimit+1; i++ {
		l.allow("198.51.100.7")
	}
	if ok, _ := l.allow("203.0.113.4"); !ok {
		t.Error("a second address was refused after the first was exhausted")
	}
}

// The address cap bounds the memory an unauthenticated route can make this
// process hold.
func TestTheAddressCapRefusesNewAddresses(t *testing.T) {
	l := newLimiterWithMax(limiterMax, 2)
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
