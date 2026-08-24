package interceptors

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// CertBinding is what a caller's row says about whether it may authenticate.
//
// One field, and it used to be four. Fingerprint, Previous and PreviousUntil
// pinned a caller to a single leaf and gave a renewal an overlap window to be
// survivable; migration 0032 removed all three when the certificate lifetime
// dropped to seven days. See that migration for the reasoning.
//
// What is left is the question the interceptor still answers faster than
// anything else: does this caller have an identity row right now. RevokeCaller
// deletes it, this is cached for five seconds, and the auth allowlist behind it
// refreshes only every thirty — so this is the path a revocation takes effect
// on.
type CertBinding struct {
	// Exists reports whether a caller_identities row is present.
	Exists bool
}

// CertBindingLookup returns the binding state for a caller.
//
// A non-nil error fails CLOSED: the interceptor logs it and refuses the call,
// the same answer an absent row gets. An earlier version of this comment called
// that "fails open", which is the opposite of what the code does and the more
// dangerous of the two to believe — a database blip refuses traffic, it does not
// admit it.
//
// Lookups MUST NOT block indefinitely; the caller is on the request hot path.
type CertBindingLookup func(ctx context.Context, caller string) (CertBinding, error)

// CertBindingConfig parameterises the cert-binding interceptor.
type CertBindingConfig struct {
	// Lookup is the DB-backed (or test-faked) resolver for whether a caller
	// still has an identity row. Required when Enforce is true.
	Lookup CertBindingLookup

	// Enforce gates the entire check. Set to true exactly when mTLS is
	// configured — without TLS there's no peer cert to bind against.
	Enforce bool

	// CallerFromContext extracts the resolved caller identity from ctx
	// (populated by resolveCallerInterceptor).
	CallerFromContext func(context.Context) string

	// ExemptCallers is the set of CNs that skip the check entirely.
	//
	// Empty by default, and the console is deliberately not in it any more.
	// Exempting a caller now means exempting it from "do you still have an
	// identity row", which is the only revocation this system has — so an
	// exempt caller is one RevokeCaller cannot cut off. That is a serious
	// thing to hand out and belongs to an operator who has a bootstrap CN
	// authenticating by some other means, not to a default.
	ExemptCallers []string

	// CacheTTL is how long a lookup result is held in process before
	// being re-read. Default 5s. Set to 0 to disable caching (tests).
	CacheTTL time.Duration

	// Log receives structured records of every reject decision plus
	// any transient lookup errors.
	Log *slog.Logger
}

// CertBindingChecker owns the cert-binding state shared across both
// the unary and stream interceptors: the TTL cache of CN -> whether an
// identity row exists, the exempt-CN set, and the resolver callbacks. Mount
// via .Unary() and .Stream() on their respective chains; both
// methods consult the same cache so a stream RPC and a unary RPC for
// the same CN share lookup state — a single DB hit per CN per TTL
// window across the whole gRPC surface, not one per interceptor
// flavor.
//
// Lifecycle: construct once at server boot; safe to call .Unary()
// and .Stream() multiple times (each returns a fresh closure, but
// all closures from the same Checker share the underlying cache and
// config).
type CertBindingChecker struct {
	check func(ctx context.Context, fullMethod string) error
}

// NewCertBindingChecker constructs the shared checker. The supplied
// CertBindingConfig is captured by value; subsequent mutations on
// the original config don't affect the checker (defense against the
// "caller secretly expanded the exempt list" footgun).
func NewCertBindingChecker(cfg CertBindingConfig) *CertBindingChecker {
	return &CertBindingChecker{check: buildCertBindingCheck(cfg)}
}

// Unary returns the unary interceptor flavor.
func (c *CertBindingChecker) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := c.check(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream returns the streaming interceptor flavor. Applied at stream
// open; the peer cert is fixed at TLS handshake time, so re-checking
// on every envelope would be wasted work.
func (c *CertBindingChecker) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := c.check(ss.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// NewCertBinding is a thin backwards-compat wrapper. New code should
// prefer NewCertBindingChecker so unary + stream callers share one
// cache. Calling NewCertBinding + NewCertBindingStream with the same
// config creates two independent caches; calling NewCertBindingChecker
// once and using .Unary() + .Stream() shares them.
func NewCertBinding(cfg CertBindingConfig) grpc.UnaryServerInterceptor {
	return NewCertBindingChecker(cfg).Unary()
}

// NewCertBindingStream is the backwards-compat sibling of NewCertBinding.
// See the note on NewCertBinding about cache sharing.
func NewCertBindingStream(cfg CertBindingConfig) grpc.StreamServerInterceptor {
	return NewCertBindingChecker(cfg).Stream()
}

// buildCertBindingCheck extracts the binding check so both
// flavors share one implementation. Returns a closure that captures
// the cache + lookup so a single TTL bucket serves the whole gRPC
// surface.
func buildCertBindingCheck(cfg CertBindingConfig) func(ctx context.Context, fullMethod string) error {
	enforce := cfg.Enforce
	callerFn := cfg.CallerFromContext
	if callerFn == nil {
		callerFn = func(context.Context) string { return "anonymous" }
	}
	exempt := make(map[string]struct{}, len(cfg.ExemptCallers))
	for _, c := range cfg.ExemptCallers {
		exempt[c] = struct{}{}
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	cache := &bindingCache{ttl: ttl}

	return func(ctx context.Context, fullMethod string) error {
		if !enforce {
			return nil
		}
		caller := callerFn(ctx)
		// Anonymous reaches us in insecure dev mode (no mTLS configured
		// for the listener). The auth interceptor will reject it; this
		// interceptor has nothing meaningful to check.
		if caller == "" || caller == "anonymous" {
			return nil
		}
		if _, ok := exempt[caller]; ok {
			return nil
		}

		// The peer certificate is still required.
		//
		// With Enforce=true the listener is tls.RequireAndVerifyClientCert, so a
		// missing peer certificate here means the listener is mis-configured —
		// fail closed rather than trust the caller name alone, which is derived
		// from that certificate.
		//
		// What is NOT done any more is comparing its fingerprint to a stored one.
		// Migration 0032 removed pinning when the certificate lifetime dropped to
		// seven days; the chain and the common name are verified by the handshake,
		// and a certificate that should not exist stops working when it expires.
		if _, ok := leafCertFromContext(ctx); !ok {
			log.Warn("cert binding: no peer cert on enforced path", "caller", caller, "method", fullMethod)
			return status.Error(codes.Unauthenticated, "no peer certificate")
		}

		b, err := cache.lookup(ctx, caller, cfg.Lookup)
		if err != nil {
			log.Error("cert binding: lookup", "caller", caller, "method", fullMethod, "err", err)
			return status.Error(codes.Unauthenticated, "caller binding unavailable")
		}
		if !b.Exists {
			// Caller has no row — either never registered, or revoked. Either way
			// it cannot authenticate, and this is the fastest path that says so:
			// five seconds behind a RevokeCaller, against thirty for the auth
			// allowlist's refresher.
			//
			// Distinguishing "never registered" from "revoked" would leak
			// existence; one error covers both.
			log.Info("cert binding: unknown caller", "caller", caller, "method", fullMethod)
			return status.Errorf(codes.Unauthenticated, "caller %q is not registered", caller)
		}
		return nil
	}
}

// forwardedCertKey carries the DER of a trusted-proxy-forwarded, already-
// re-validated end-client cert. When present it is the cert the binding check
// treats as the caller's — so the check is satisfied by the end client's
// certificate rather than the proxy's through a TLS-terminating edge.
type forwardedCertKey struct{}

// WithForwardedCert stashes the re-validated forwarded client cert DER on
// the context. Set by the server's resolve interceptor only after the cert
// has been verified (chain + clientAuth EKU + validity); this package treats
// its presence as authoritative for which certificate the check sees.
func WithForwardedCert(ctx context.Context, der []byte) context.Context {
	return context.WithValue(ctx, forwardedCertKey{}, der)
}

// ForwardedCertFromContext returns the forwarded client cert DER, if any. A
// present value also marks the request as trusted-proxy-forwarded, which the
// admin gates consult.
func ForwardedCertFromContext(ctx context.Context) ([]byte, bool) {
	der, ok := ctx.Value(forwardedCertKey{}).([]byte)
	return der, ok && len(der) > 0
}

// leafCertFromContext extracts the cert the binding check requires to be
// present: the trusted-proxy-forwarded cert when there is one (so the check
// survives edge TLS termination), otherwise the live TLS peer's leaf cert.
// Returns false in non-TLS modes (where no cert was negotiated) so callers can
// react.
func leafCertFromContext(ctx context.Context) (cert leafCert, ok bool) {
	if der, fwd := ForwardedCertFromContext(ctx); fwd {
		return leafCert{Raw: der}, true
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return leafCert{}, false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return leafCert{}, false
	}
	if len(info.State.PeerCertificates) == 0 {
		return leafCert{}, false
	}
	return leafCert{Raw: info.State.PeerCertificates[0].Raw}, true
}

// leafCert is a tiny shim over *x509.Certificate so the interceptor
// only depends on the field it actually uses — the raw DER bytes. Nothing
// hashes them now; the check is that a verified certificate was presented
// at all, which is what distinguishes a caller from a mis-configured listener.
type leafCert struct {
	Raw []byte
}

// bindingCache is a per-process TTL cache of (caller → does an identity row
// exist). One DB read per CN per TTL window under burst — at our QPS
// floor this is the difference between "Postgres absorbs the load"
// and "Postgres becomes the bottleneck."
//
// Stale-while-revalidate is intentionally NOT implemented: the TTL is
// short (5s default) and rotation/revoke decisions are operator-paced
// — extra freshness machinery would be all cost, no benefit. Operators
// who need immediate effect can restart the server or wait one TTL.
type bindingCache struct {
	mu  sync.RWMutex
	m   map[string]bindingEntry
	ttl time.Duration
}

type bindingEntry struct {
	binding CertBinding
	expires time.Time
}

func (c *bindingCache) lookup(ctx context.Context, caller string, fn CertBindingLookup) (CertBinding, error) {
	now := time.Now()
	c.mu.RLock()
	e, ok := c.m[caller]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.binding, nil
	}
	b, err := fn(ctx, caller)
	if err != nil {
		return CertBinding{}, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]bindingEntry)
	}
	c.m[caller] = bindingEntry{binding: b, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return b, nil
}
