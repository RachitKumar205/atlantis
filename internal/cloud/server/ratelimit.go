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

// Rate-limiter bounds.
//
// Ported from the console's loginLimiter (internal/console/server.go) rather
// than written again, because the two are solving the same problem and a second
// implementation is a second thing to get wrong. What differs is what it
// protects: the console throttles assertion exchange, this throttles sign-up
// and password-reset requests.
//
// Sign-up and reset are both a request that causes an email to be sent to an
// address the requester chose. Without a limit that is a mail cannon pointed at
// anyone, using our sending reputation.
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
}

func newLimiter() *limiter {
	return &limiter{hits: make(map[string][]time.Time), lastSwp: time.Now(), now: time.Now}
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

	if len(hits) >= limiterMax {
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
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// The latency floor.
//
// # Why a floor and not just identical responses
//
// Sign-up and reset-request answer the same way whether or not the address has
// an account — that is what stops the response body being an oracle. It is not
// enough on its own, because the WORK differs: a registered address means a
// database write, a token, and an email; an unregistered one means none of
// those. The difference is tens of milliseconds and is trivially measurable
// from outside, so the body says nothing and the clock says everything.
//
// Holding every response to a fixed floor removes the signal. The floor has to
// exceed the slow path, or the slow path pokes through it and the defence
// silently stops working for exactly the requests that matter.
const latencyFloor = 500 * time.Millisecond

// latencyJitter is added on top so the floor itself is not a clean edge.
//
// Without it every response lands at almost exactly the floor, and a slow path
// that overruns is visible as the one response that does not. A few tens of
// milliseconds of noise costs nothing and removes that.
const latencyJitter = 50 * time.Millisecond

// floorLatency waits until at least the floor has passed since start.
//
// Takes the context so a client that has gone away does not hold the handler
// for the remainder — and so tests do not sleep.
func floorLatency(ctx context.Context, start time.Time, sleep func(context.Context, time.Duration)) {
	target := latencyFloor + time.Duration(rand.Int64N(int64(latencyJitter)))
	if elapsed := time.Since(start); elapsed < target {
		sleep(ctx, target-elapsed)
	}
}

// realSleep is the production sleeper. Tests substitute one that records the
// requested duration instead of waiting, so the property can be asserted
// without adding half a second per case.
func realSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
