package interceptors

import (
	"context"
	"testing"

	"google.golang.org/grpc/status"
)

// A trusted-proxy-forwarded certificate satisfies the peer-certificate
// requirement, so a caller behind a TLS-terminating proxy is not refused for
// having no live peer.
//
// This used to assert that the check FINGERPRINTED the forwarded certificate
// rather than the live peer, which was the whole point while callers were
// pinned to one leaf. Migration 0032 removed pinning, so there is no
// fingerprint to compare and the surviving property is the one below: the
// forwarded certificate is what leafCertFromContext finds.
//
// Still worth a test. Without it the interceptor would refuse every proxied
// caller with "no peer certificate", which reads as a TLS problem and is not.
func TestCertBinding_ForwardedCertSatisfiesThePeerRequirement(t *testing.T) {
	der := []byte("re-validated-forwarded-cert-der")
	callerFn := func(context.Context) string { return "vendor" }

	t.Run("a registered caller behind a proxy passes", func(t *testing.T) {
		chk := NewCertBindingChecker(CertBindingConfig{
			Enforce:           true,
			CallerFromContext: callerFn,
			Lookup: func(context.Context, string) (CertBinding, error) {
				return CertBinding{Exists: true}, nil
			},
		})
		ctx := WithForwardedCert(context.Background(), der)
		if err := chk.check(ctx, "/x/y"); err != nil {
			t.Fatalf("a forwarded certificate should satisfy the peer requirement, got %v", err)
		}
	})

	t.Run("no certificate at all is still refused", func(t *testing.T) {
		chk := NewCertBindingChecker(CertBindingConfig{
			Enforce:           true,
			CallerFromContext: callerFn,
			Lookup: func(context.Context, string) (CertBinding, error) {
				return CertBinding{Exists: true}, nil
			},
		})
		// No forwarded cert and no peer: the listener is mis-configured, and the
		// caller name is derived from a certificate that is not there.
		if err := chk.check(context.Background(), "/x/y"); status.Code(err) == 0 || err == nil {
			t.Fatalf("a request with no certificate must be refused, got %v", err)
		}
	})

	t.Run("a revoked caller is refused even behind a proxy", func(t *testing.T) {
		chk := NewCertBindingChecker(CertBindingConfig{
			Enforce:           true,
			CallerFromContext: callerFn,
			Lookup: func(context.Context, string) (CertBinding, error) {
				return CertBinding{Exists: false}, nil
			},
		})
		ctx := WithForwardedCert(context.Background(), der)
		if err := chk.check(ctx, "/x/y"); status.Code(err) == 0 || err == nil {
			t.Fatalf("a caller with no identity row must be refused, got %v", err)
		}
	})
}

func TestForwardedCertContextRoundTrip(t *testing.T) {
	if _, ok := ForwardedCertFromContext(context.Background()); ok {
		t.Fatal("bare context should report no forwarded cert")
	}
	der := []byte("der")
	ctx := WithForwardedCert(context.Background(), der)
	got, ok := ForwardedCertFromContext(ctx)
	if !ok || string(got) != "der" {
		t.Fatalf("round-trip failed: ok=%v got=%q", ok, got)
	}
	// An empty DER must not mark the request forwarded.
	if _, ok := ForwardedCertFromContext(WithForwardedCert(context.Background(), nil)); ok {
		t.Error("empty forwarded DER should not mark the request forwarded")
	}
}
