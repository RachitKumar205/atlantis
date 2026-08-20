package interceptors

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Stream test scaffolding
// ---------------------------------------------------------------------------

// fakeServerStream implements grpc.ServerStream just enough for the
// interceptor tests. Context() returns the injected ctx; the rest are
// safe no-ops because the interceptors under test don't call them.
type fakeServerStream struct{ ctx context.Context }

func (s *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeServerStream) SetTrailer(metadata.MD)       {}
func (s *fakeServerStream) Context() context.Context     { return s.ctx }
func (s *fakeServerStream) SendMsg(any) error            { return nil }
func (s *fakeServerStream) RecvMsg(any) error            { return nil }

// noopStreamHandler returns nil — the interceptors under test either
// reject before reaching the handler or pass through to it; either
// way the handler's body is uninteresting.
func noopStreamHandler(_ any, _ grpc.ServerStream) error { return nil }

func runAuthStream(t *testing.T, cfg AuthConfig, method, caller string) error {
	t.Helper()
	cfg.CallerFromContext = callerFrom
	intr := NewAuthChecker(cfg).Stream()
	info := &grpc.StreamServerInfo{FullMethod: method}
	ss := &fakeServerStream{ctx: callCtx(caller)}
	return intr(nil, ss, info, noopStreamHandler)
}

// ---------------------------------------------------------------------------
// AuthChecker.Stream tests — mirror the unary tests so the same
// allowlist semantics are confirmed on both surfaces.
// ---------------------------------------------------------------------------

func TestAuthStream_DisabledIsNoOp(t *testing.T) {
	if err := runAuthStream(t, AuthConfig{Enforce: false}, "/x.Y/Z", "anonymous"); err != nil {
		t.Errorf("disabled stream auth should pass: %v", err)
	}
}

func TestAuthStream_ExemptPrefixBypasses(t *testing.T) {
	cfg := AuthConfig{
		Enforce:        true,
		Allowlist:      stubAllower{},
		ExemptPrefixes: []string{"/atlantis.admin.v1.Admin/"},
	}
	if err := runAuthStream(t, cfg, "/atlantis.admin.v1.Admin/PlanSchema", "newcomer"); err != nil {
		t.Errorf("admin prefix exempt on stream: %v", err)
	}
}

func TestAuthStream_AnonymousRejected(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	err := runAuthStream(t, cfg, "/x.Y/Z", "anonymous")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("anonymous on stream should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthStream_RegisteredCallerPasses(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	if err := runAuthStream(t, cfg, "/x.Y/Z", "alice"); err != nil {
		t.Errorf("registered caller stream rejected: %v", err)
	}
}

func TestAuthStream_UnregisteredCallerRejected(t *testing.T) {
	cfg := AuthConfig{Enforce: true, Allowlist: stubAllower{"alice": {}}}
	err := runAuthStream(t, cfg, "/x.Y/Z", "mallory")
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("unregistered stream caller should be PermissionDenied, got %v", status.Code(err))
	}
}

// ---------------------------------------------------------------------------
// CertBindingChecker tests — covers both Unary and Stream flavors plus
// the shared-cache property.
// ---------------------------------------------------------------------------

// peerCtx builds a context with a fake mTLS peer presenting the
// supplied raw cert bytes. Matches the layout leafCertFromContext
// reads (peer.Peer → credentials.TLSInfo → tls.ConnectionState).
func peerCtx(parent context.Context, rawCertBytes []byte) context.Context {
	cert := &x509.Certificate{Raw: rawCertBytes}
	p := &peer.Peer{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}},
		},
	}
	return peer.NewContext(parent, p)
}

type cbLookupResult struct {
	exists bool
	err    error
}

type recordingLookup struct {
	results map[string]cbLookupResult
	calls   atomic.Int64
}

func (r *recordingLookup) lookup(_ context.Context, caller string) (CertBinding, error) {
	r.calls.Add(1)
	got, ok := r.results[caller]
	if !ok {
		return CertBinding{}, nil
	}
	return CertBinding{Exists: got.exists}, got.err
}

func runCertBindingUnary(t *testing.T, c *CertBindingChecker, ctx context.Context, method string) error {
	t.Helper()
	info := &grpc.UnaryServerInfo{FullMethod: method}
	_, err := c.Unary()(ctx, nil, info, okHandler)
	return err
}

func runCertBindingStream(t *testing.T, c *CertBindingChecker, ctx context.Context, method string) error {
	t.Helper()
	info := &grpc.StreamServerInfo{FullMethod: method}
	ss := &fakeServerStream{ctx: ctx}
	return c.Stream()(nil, ss, info, noopStreamHandler)
}

func TestCertBinding_DisabledIsNoOp(t *testing.T) {
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           false,
		CallerFromContext: callerFrom,
	})
	if err := runCertBindingStream(t, c, callCtx("alice"), "/x.Y/Z"); err != nil {
		t.Errorf("disabled stream cert binding should pass: %v", err)
	}
	if err := runCertBindingUnary(t, c, callCtx("alice"), "/x.Y/Z"); err != nil {
		t.Errorf("disabled unary cert binding should pass: %v", err)
	}
}

func TestCertBinding_AnonymousSkipped(t *testing.T) {
	// Anonymous reaches this interceptor in insecure dev mode. Auth
	// will reject downstream; this interceptor has nothing to do.
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            (&recordingLookup{}).lookup,
		CallerFromContext: callerFrom,
	})
	if err := runCertBindingStream(t, c, callCtx("anonymous"), "/x.Y/Z"); err != nil {
		t.Errorf("anonymous stream should skip cert binding, got %v", err)
	}
}

func TestCertBinding_ExemptCallerSkipped(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
		ExemptCallers:     []string{"atlantis-console"},
	})
	if err := runCertBindingStream(t, c, callCtx("atlantis-console"), "/x.Y/Z"); err != nil {
		t.Errorf("exempt caller on stream should pass without lookup: %v", err)
	}
	if rl.calls.Load() != 0 {
		t.Errorf("exempt caller should not trigger lookup, got %d calls", rl.calls.Load())
	}
}

func TestCertBinding_NoPeerCertRejected(t *testing.T) {
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            (&recordingLookup{}).lookup,
		CallerFromContext: callerFrom,
	})
	// Caller is set but no peer cert in context → listener mis-configured.
	err := runCertBindingStream(t, c, callCtx("alice"), "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("missing peer cert should be Unauthenticated, got %v", status.Code(err))
	}
}

func TestCertBinding_UnknownCallerRejected(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), []byte("any-cert"))
	err := runCertBindingStream(t, c, ctx, "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("unknown caller should be Unauthenticated, got %v", status.Code(err))
	}
}

// Any certificate that passed the handshake is accepted for a registered
// caller.
//
// This replaces three tests — a "bootstrap window" that accepted anything until
// the first fingerprint was recorded, a mismatch rejection, and a match pass.
// All three described pinning, which migration 0032 removed when the leaf
// lifetime dropped to seven days.
//
// The bootstrap window is the one worth remembering: it accepted ANY CA-signed
// certificate for a caller whose fingerprint was NULL, and because the only
// writer of that column never ran in any deployment, every caller everywhere sat
// in it permanently. The behaviour below is what that window actually was, now
// made deliberate and bounded by a short certificate rather than by an accident.
func TestCertBinding_AnyValidCertPassesForARegisteredCaller(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	// Two different certificates, both fine: the handshake already established
	// that each chains to the authority and names this caller.
	for _, raw := range [][]byte{[]byte("first-cert"), []byte("renewed-cert")} {
		ctx := peerCtx(callCtx("alice"), raw)
		if err := runCertBindingStream(t, c, ctx, "/x.Y/Z"); err != nil {
			t.Errorf("a valid certificate for a registered caller was refused: %v", err)
		}
	}
}

// Revocation still takes effect here, and this is the fastest path it has.
//
// RevokeCaller deletes the identity row; this check is cached for five seconds,
// against thirty for the auth allowlist's refresher. Dropping pinning did not
// change that, and it is the reason this interceptor still exists.
func TestCertBinding_ARevokedCallerIsRefusedWithAValidCert(t *testing.T) {
	rl := &recordingLookup{results: map[string]cbLookupResult{}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
		CacheTTL:          time.Nanosecond,
	})
	ctx := peerCtx(callCtx("alice"), []byte("a-perfectly-good-cert"))
	if err := runCertBindingStream(t, c, ctx, "/x.Y/Z"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a caller with no identity row was admitted: %v", status.Code(err))
	}
}

func TestCertBinding_LookupErrorRejected(t *testing.T) {
	// Transient lookup failures fail closed — caller can't auth via
	// a broken DB connection.
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {err: errors.New("db down")},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	})
	ctx := peerCtx(callCtx("alice"), []byte("any-cert"))
	err := runCertBindingStream(t, c, ctx, "/x.Y/Z")
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("lookup error should fail closed Unauthenticated, got %v", status.Code(err))
	}
}

// TestCertBinding_SharedCache pins the key promise of the Checker
// refactor: one Checker → shared cache across Unary() and Stream().
// Two interceptor calls for the same CN must hit the DB exactly
// once, regardless of which flavor opened the first call.
func TestCertBinding_SharedCacheAcrossUnaryAndStream(t *testing.T) {
	rawCert := []byte("shared-cert")
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true},
	}}
	c := NewCertBindingChecker(CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
		CacheTTL:          5 * time.Second,
	})
	ctx := peerCtx(callCtx("alice"), rawCert)

	if err := runCertBindingUnary(t, c, ctx, "/unary.Path"); err != nil {
		t.Fatalf("unary call: %v", err)
	}
	if err := runCertBindingStream(t, c, ctx, "/stream.Path"); err != nil {
		t.Fatalf("stream call: %v", err)
	}
	if err := runCertBindingUnary(t, c, ctx, "/unary.Path"); err != nil {
		t.Fatalf("second unary call: %v", err)
	}
	if got := rl.calls.Load(); got != 1 {
		t.Errorf("shared cache should cause exactly 1 DB lookup; got %d", got)
	}
}

// TestCertBinding_SeparateCachesWithLegacyAPI confirms that the
// backwards-compat NewCertBinding + NewCertBindingStream pattern
// (used to be the only API) does NOT share a cache. Documents the
// behavior so a future reader knows the Checker pattern is the
// path to deduplication.
func TestCertBinding_SeparateCachesWithLegacyAPI(t *testing.T) {
	rawCert := []byte("legacy-cert")
	rl := &recordingLookup{results: map[string]cbLookupResult{
		"alice": {exists: true},
	}}
	cfg := CertBindingConfig{
		Enforce:           true,
		Lookup:            rl.lookup,
		CallerFromContext: callerFrom,
	}

	unary := NewCertBinding(cfg)        // legacy wrapper → its own cache
	stream := NewCertBindingStream(cfg) // legacy wrapper → another cache

	ctx := peerCtx(callCtx("alice"), rawCert)
	unaryInfo := &grpc.UnaryServerInfo{FullMethod: "/u.X/Y"}
	streamInfo := &grpc.StreamServerInfo{FullMethod: "/s.X/Y"}

	if _, err := unary(ctx, nil, unaryInfo, okHandler); err != nil {
		t.Fatalf("legacy unary: %v", err)
	}
	if err := stream(nil, &fakeServerStream{ctx: ctx}, streamInfo, noopStreamHandler); err != nil {
		t.Fatalf("legacy stream: %v", err)
	}

	// Two separate caches → two separate DB hits. Documenting, not
	// asserting "should be 2" rigidly — what matters is that the
	// shared-cache test above passes with the Checker API.
	if got := rl.calls.Load(); got < 2 {
		t.Errorf("legacy API should NOT dedupe; expected >=2 lookups, got %d", got)
	}
}

// TestCertBinding_CertBindingChecker_StreamMatchesUnary confirms
// the two flavors agree on the verdict for every input. Important
// because the security contract relies on this equivalence — drift
// between flavors would let an attacker fail a unary check then
// retry on a stream.
func TestCertBinding_StreamMatchesUnary(t *testing.T) {
	rawCert := []byte("alice-cert")
	cfg := CertBindingConfig{
		Enforce: true,
		Lookup: (&recordingLookup{results: map[string]cbLookupResult{
			"alice":   {exists: true},
			"mallory": {exists: false},
		}}).lookup,
		CallerFromContext: callerFrom,
	}

	cases := []struct {
		name     string
		ctx      context.Context
		wantCode codes.Code
	}{
		{"valid", peerCtx(callCtx("alice"), rawCert), codes.OK},
		// Was "wrong_fingerprint", expecting Unauthenticated. A renewed
		// certificate for a registered caller is now accepted — that is what
		// dropping pinning means, and the two flavours must agree about it just
		// as they agreed about refusing it.
		{"renewed_cert", peerCtx(callCtx("alice"), []byte("rotated")), codes.OK},
		{"unknown_caller", peerCtx(callCtx("mallory"), rawCert), codes.Unauthenticated},
		{"no_peer_cert", callCtx("alice"), codes.Unauthenticated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each iteration gets a fresh checker so the cache doesn't
			// hide differences between unary and stream code paths.
			c1 := NewCertBindingChecker(cfg)
			c2 := NewCertBindingChecker(cfg)
			unaryErr := runCertBindingUnary(t, c1, tc.ctx, "/u.X/Y")
			streamErr := runCertBindingStream(t, c2, tc.ctx, "/s.X/Y")
			if status.Code(unaryErr) != tc.wantCode {
				t.Errorf("unary code = %v, want %v", status.Code(unaryErr), tc.wantCode)
			}
			if status.Code(streamErr) != tc.wantCode {
				t.Errorf("stream code = %v, want %v", status.Code(streamErr), tc.wantCode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WithStreamContext + ctxStream wrapper test
// ---------------------------------------------------------------------------

// TestWithStreamContext_PropagatesValue pins that the ctxStream
// wrapper actually returns the supplied context — the whole point
// of the stream-resolveCaller interceptor relies on this.
func TestWithStreamContext_PropagatesValue(t *testing.T) {
	type k struct{}
	base := &fakeServerStream{ctx: context.Background()}
	wrapped := WithStreamContext(base, context.WithValue(context.Background(), k{}, "v"))
	got, _ := wrapped.Context().Value(k{}).(string)
	if got != "v" {
		t.Errorf("WithStreamContext: got value %q, want %q", got, "v")
	}
}

// ---------------------------------------------------------------------------
// AuthChecker StreamMatchesUnary
// ---------------------------------------------------------------------------

// Equivalent to the cert binding cross-flavor test: both flavors of
// the auth checker must reach identical verdicts for every input.
func TestAuth_StreamMatchesUnary(t *testing.T) {
	cfg := AuthConfig{
		Enforce:           true,
		Allowlist:         stubAllower{"alice": {}},
		ExemptPrefixes:    []string{"/atlantis.admin.v1.Admin/"},
		CallerFromContext: callerFrom,
	}

	cases := []struct {
		name     string
		caller   string
		method   string
		wantCode codes.Code
	}{
		{"in_allowlist", "alice", "/x.Y/Z", codes.OK},
		{"not_in_allowlist", "mallory", "/x.Y/Z", codes.PermissionDenied},
		{"anonymous", "anonymous", "/x.Y/Z", codes.Unauthenticated},
		{"exempt_prefix_for_unregistered", "mallory", "/atlantis.admin.v1.Admin/Foo", codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewAuthChecker(cfg)
			unaryInfo := &grpc.UnaryServerInfo{FullMethod: tc.method}
			streamInfo := &grpc.StreamServerInfo{FullMethod: tc.method}
			_, unaryErr := c.Unary()(callCtx(tc.caller), nil, unaryInfo, okHandler)
			streamErr := c.Stream()(nil, &fakeServerStream{ctx: callCtx(tc.caller)}, streamInfo, noopStreamHandler)
			if status.Code(unaryErr) != tc.wantCode {
				t.Errorf("unary code = %v, want %v", status.Code(unaryErr), tc.wantCode)
			}
			if status.Code(streamErr) != tc.wantCode {
				t.Errorf("stream code = %v, want %v", status.Code(streamErr), tc.wantCode)
			}
		})
	}
}
