package server

import (
	"context"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Rate-limiter bounds. Ported from the console's loginLimiter
// (internal/console/server.go), which throttles assertion exchange.
//
// Sign-up and reset both send mail to an address the requester chose, so
// without a limit they relay mail under this deployment's sending reputation.
//
// Memory bound: at most limiterMaxIPs entries of at most limiterMax timestamps.
const (
	limiterMax      = 5           // requests per window per IP
	limiterWindow   = time.Minute // sliding window
	limiterMaxIPs   = 10_000      // cap to bound memory
	limiterSweepAge = 10 * time.Minute
)

type limiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	lastSwp time.Time
	now     func() time.Time

	// max requests per window per key. limiterMax for the auth routes; the
	// CLI poll limiter keys on a device-code hash rather than an address and
	// carries a budget sized to its polling interval.
	max int
}

func newLimiter() *limiter {
	return newLimiterWithMax(limiterMax)
}

func newLimiterWithMax(max int) *limiter {
	return &limiter{hits: make(map[string][]time.Time), lastSwp: time.Now(), now: time.Now, max: max}
}

// allow reports whether this address may make another request, and how many
// seconds until it may.
func (l *limiter) allow(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-limiterWindow)

	// Periodic sweep so the map does not grow forever after an attack.
	if now.Sub(l.lastSwp) > limiterSweepAge {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(now.Add(-limiterSweepAge)) {
				delete(l.hits, k)
			}
		}
		l.lastSwp = now
	}

	// Hard cap on tracked addresses. Full and unknown means refused: failing
	// closed is better than silently amnestying everyone under load, which is
	// exactly when the limit matters.
	if len(l.hits) >= limiterMaxIPs {
		if _, known := l.hits[ip]; !known {
			return false, int(limiterWindow.Seconds())
		}
	}

	hits := l.hits[ip]
	idx := 0
	for ; idx < len(hits); idx++ {
		if hits[idx].After(cutoff) {
			break
		}
	}
	hits = hits[idx:]

	if len(hits) >= l.max {
		l.hits[ip] = hits
		retry := int(limiterWindow.Seconds() - now.Sub(hits[0]).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}

	l.hits[ip] = append(hits, now)
	return true, 0
}

// clientIP is the key the limiter counts against.
//
// X-Forwarded-For is only read when the deployment says something it controls
// terminates in front. Otherwise the header is attacker-supplied, and a limiter
// keyed on it is one an attacker resets on every request by changing a string.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := forwardedClientIP(r.Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedClientIP returns the address the terminator observed, or "" when
// the header carries none.
//
// Google's external load balancer APPENDS `<client>,<lb>` to whatever arrived,
// so the leftmost element is a value the caller chose. Reading it makes the
// limiter resettable per request by changing a string, which is what trusting
// the header is supposed to prevent. The client is second from the end.
//
// Anything that does not parse as an address is refused, so a key is bounded
// in both length and count.
func forwardedClientIP(xff string) string {
	if xff == "" {
		return ""
	}
	parts := strings.Split(xff, ",")
	i := len(parts) - 2
	if i < 0 {
		i = 0
	}
	ip := strings.TrimSpace(parts[i])
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// The latency floor. A registered address costs a database write, a token and
// an email; an unregistered one costs none of those, a difference of tens of
// milliseconds that is measurable from outside.
//
// The floor must exceed the slow path. A slow path that overruns it shows
// through as the response that took longer.
const latencyFloor = 500 * time.Millisecond

// latencyJitter is added on top so responses do not land on a clean edge at the
// floor, where an overrunning slow path is the one that misses it.
const latencyJitter = 50 * time.Millisecond

// floorLatency waits until at least the floor has passed since start.
//
// Takes the context so a client that has gone away does not hold the handler,
// and so tests can substitute a sleeper.
func floorLatency(ctx context.Context, start time.Time, sleep func(context.Context, time.Duration)) {
	target := latencyFloor + time.Duration(rand.Int64N(int64(latencyJitter)))
	if elapsed := time.Since(start); elapsed < target {
		sleep(ctx, target-elapsed)
	}
}

// realSleep is the production sleeper. Tests substitute one that records the
// requested duration instead of waiting.
func realSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
