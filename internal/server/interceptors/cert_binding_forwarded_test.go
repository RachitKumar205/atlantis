package interceptors

import (
	"context"
	"crypto/sha256"
	"testing"

	"google.golang.org/grpc/status"
)

// When a trusted-proxy-forwarded cert is on the context, the binding check
// fingerprints THAT cert (not the live peer), so per-caller cert binding is
// preserved through a TLS-terminating proxy.
func TestCertBinding_ForwardedCertIsTheFingerprintSource(t *testing.T) {
	der := []byte("re-validated-forwarded-cert-der")
	fp := sha256.Sum256(der)
	callerFn := func(context.Context) string { return "vendor" }

	t.Run("matching forwarded fingerprint passes", func(t *testing.T) {
		chk := NewCertBindingChecker(CertBindingConfig{
			Enforce:           true,
			CallerFromContext: callerFn,
			Lookup: func(context.Context, string) (bool, []byte, error) {
				return true, fp[:], nil
			},
		})
		ctx := WithForwardedCert(context.Background(), der)
		if err := chk.check(ctx, "/x/y"); err != nil {
			t.Fatalf("forwarded cert matching the stored fingerprint should pass, got %v", err)
		}
	})

	t.Run("mismatched forwarded fingerprint is rejected", func(t *testing.T) {
		other := sha256.Sum256([]byte("some-other-cert"))
		chk := NewCertBindingChecker(CertBindingConfig{
			Enforce:           true,
			CallerFromContext: callerFn,
			Lookup: func(context.Context, string) (bool, []byte, error) {
				return true, other[:], nil
			},
		})
		ctx := WithForwardedCert(context.Background(), der)
		err := chk.check(ctx, "/x/y")
		if status.Code(err) == 0 || err == nil {
			t.Fatalf("forwarded cert not matching the stored fingerprint must be rejected, got %v", err)
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
