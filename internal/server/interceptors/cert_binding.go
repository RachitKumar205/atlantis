package interceptors

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// RenewalOverlap is how long a superseded certificate keeps working after the
// one replacing it is recorded.
//
// It lives here because two packages have to agree on it and neither may import
// the other: internal/server/admin sets prev_valid_until from it when it records
// a renewal, and internal/console decides from it whether a machine presenting a
// superseded certificate may renew again. If those two drifted apart, the gap
// would be a window in which a machine can authenticate but cannot renew — a
// lockout that appears only for certificates in exactly that band.
//
// Sized against what it is for: a machine that has just been handed a
// certificate and needs to store it. That takes milliseconds, and the failures
// worth surviving — a lost response, a crash before the file lands, a retry
// through a queue — resolve in minutes. Twenty-four hours is generous by orders
// of magnitude, deliberately: too short locks a caller out with no self-service
// recovery, and too long leaves a stolen certificate alive one extra day inside
// a 90-day life it already had.
const RenewalOverlap = 24 * time.Hour

// CertBinding is what a caller's row says about which certificates
// authenticate as it.
type CertBinding struct {
	// Exists reports whether a caller_identities row is present.
	Exists bool

	// Fingerprint is the 32-byte SHA-256 of the currently-active cert, or
	// nil when the row exists and no cert has been recorded — the
	// back-compat branch for callers minted before binding existed.
	Fingerprint []byte

	// Previous is the certificate this one replaced, and PreviousUntil is
	// when it stops being accepted. Both set or neither; a database CHECK
	// says so (migration 0031).
	//
	// The window exists because a renewal's response can be lost after the
	// new fingerprint is written — a timeout, a 502, a crash before the file
	// reaches disk. Without it the machine holds a superseded certificate and
	// needs a valid one to renew, which is a lockout only an operator can
	// undo, on a path `tide login` walks unattended across a fleet.
	Previous      []byte
	PreviousUntil time.Time
}

// CertBindingLookup returns the binding state for a caller.
//
// Returning a non-nil error fails open in the same way the lookup
// would if the row truly didn't exist — the interceptor logs the
// error and treats it as "unknown caller." Lookups MUST NOT block
// indefinitely; the caller is on the request hot path.
type CertBindingLookup func(ctx context.Context, caller string) (CertBinding, error)

// CertBindingConfig parameterises the cert-binding interceptor.
type CertBindingConfig struct {
	// Lookup is the DB-backed (or test-faked) resolver for a caller's
	// stored fingerprint. Required when Enforce is true.
	Lookup CertBindingLookup

	// Enforce gates the entire check. Set to true exactly when mTLS is
	// configured — without TLS there's no peer cert to bind against.
	Enforce bool

	// CallerFromContext extracts the resolved caller identity from ctx
	// (populated by resolveCallerInterceptor).
	CallerFromContext func(context.Context) string

	// ExemptCallers is the set of CNs that always pass without a
	// fingerprint check. Reserved for the management plane (the console
	// CN, the signer CN) whose authentication is enforced at a higher
	// layer (session cookies + sudo + role).
	ExemptCallers []string

	// CacheTTL is how long a lookup result is held in process before
	// being re-read. Default 5s. Set to 0 to disable caching (tests).
	CacheTTL time.Duration

	// Log receives structured records of every reject decision plus
	// any transient lookup errors.
	Log *slog.Logger
}

// CertBindingChecker owns the cert-binding state shared across both
// the unary and stream interceptors: the TTL cache of CN -> stored
// fingerprint, the exempt-CN set, and the resolver callbacks. Mount
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

// buildCertBindingCheck extracts the fingerprint comparison so both
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

		// Pull the presented leaf cert. With Enforce=true the listener
		// is tls.RequireAndVerifyClientCert, so a missing peer cert
		// here means the listener is mis-configured — fail closed.
		peerCert, ok := leafCertFromContext(ctx)
		if !ok {
			log.Warn("cert binding: no peer cert on enforced path", "caller", caller, "method", fullMethod)
			return status.Error(codes.Unauthenticated, "no peer certificate")
		}
		presented := sha256.Sum256(peerCert.Raw)

		b, err := cache.lookup(ctx, caller, cfg.Lookup)
		if err != nil {
			log.Error("cert binding: lookup", "caller", caller, "method", fullMethod, "err", err)
			return status.Error(codes.Unauthenticated, "caller binding unavailable")
		}
		if !b.Exists {
			// Caller has no row — either never registered, or revoked.
			// Either way it can't authenticate. Distinguishing the two
			// would leak existence; one error code covers both.
			log.Info("cert binding: unknown caller", "caller", caller, "method", fullMethod)
			return status.Errorf(codes.Unauthenticated, "caller %q is not registered", caller)
		}
		if b.Fingerprint == nil {
			// Bootstrap window: row exists but no fingerprint recorded
			// yet (operator registered the caller, hasn't enrolled a
			// machine through the console). Accept any CA-signed cert
			// until the first enrolment binds the fingerprint.
			return nil
		}
		// subtle.ConstantTimeCompare so a timing oracle can't probe
		// fingerprint bytes one column at a time.
		if subtle.ConstantTimeCompare(b.Fingerprint, presented[:]) == 1 {
			return nil
		}

		// The certificate the current one replaced, inside its overlap window.
		//
		// A renewal writes the new fingerprint before the machine can possibly
		// have stored the certificate, so between those two moments the machine
		// is still presenting the old one. Refusing it there is a lockout: it
		// needs a valid certificate to renew and no longer has one.
		//
		// Bounded by PreviousUntil rather than by "has the new one been used
		// yet", because observing first use means writing from this function —
		// the hottest read path in the product, behind a cache that would let
		// the write fire repeatedly before its effect was visible.
		if b.Previous != nil && time.Now().Before(b.PreviousUntil) &&
			subtle.ConstantTimeCompare(b.Previous, presented[:]) == 1 {
			log.Info("cert binding: accepted the superseded certificate inside its renewal window",
				"caller", caller,
				"method", fullMethod,
				"window_ends", b.PreviousUntil.UTC().Format(time.RFC3339),
			)
			return nil
		}

		log.Info("cert binding: fingerprint mismatch (cert superseded)",
			"caller", caller,
			"method", fullMethod,
		)
		return status.Errorf(codes.Unauthenticated, "cert superseded for caller %q", caller)
	}
}

// forwardedCertKey carries the DER of a trusted-proxy-forwarded, already-
// re-validated end-client cert. When present it is the cert the binding
// check fingerprints — so per-caller cert binding is preserved through a
// TLS-terminating proxy.
type forwardedCertKey struct{}

// WithForwardedCert stashes the re-validated forwarded client cert DER on
// the context. Set by the server's resolve interceptor only after the cert
// has been verified (chain + clientAuth EKU + validity); this package treats
// its presence as authoritative for the fingerprint source.
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

// leafCertFromContext extracts the cert the binding check fingerprints: the
// trusted-proxy-forwarded cert when present (so binding survives edge TLS
// termination), otherwise the live TLS peer's leaf cert. Returns false in
// non-TLS modes (where no cert was negotiated) so callers can react.
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
// only depends on the field it actually uses — the raw DER bytes the
// fingerprint hash is computed from.
type leafCert struct {
	Raw []byte
}

// bindingCache is a per-process TTL cache of (caller → fingerprint
// state). One DB read per CN per TTL window under burst — at our QPS
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
