package admin

import (
	"context"
	"strings"
	"testing"
)

// proxySvc builds a Service wired for the trusted-proxy admin-plane gates.
// allowMutation=true keeps the downstream mutation/operator checks off the
// DB so these tests isolate the forwarded-identity gate (nil pool is never
// touched on these paths).
func proxySvc(forwarded, mayApply, mayOperate bool) *Service {
	return New(nil, Config{
		AllowApplyMutation:        true,
		CallerFromContext:         func(context.Context) string { return "vendor" },
		ProxyForwardedFromContext: func(context.Context) bool { return forwarded },
		TrustedProxyMayApply:      mayApply,
		TrustedProxyMayOperate:    mayOperate,
	})
}

func TestProxyForwarded_SelfApplyGate(t *testing.T) {
	ctx := context.Background()

	// Forwarded + may-apply OFF → denied.
	err := proxySvc(true, false, false).authorizeSelfApply(ctx, "vendor")
	if err == nil || !strings.Contains(err.Error(), "trusted front proxy") {
		t.Fatalf("forwarded self-apply with may-apply off must be denied, got %v", err)
	}

	// Forwarded + may-apply ON → allowed (the normal caller workflow).
	if err := proxySvc(true, true, false).authorizeSelfApply(ctx, "vendor"); err != nil {
		t.Fatalf("forwarded self-apply with may-apply on should pass, got %v", err)
	}

	// Direct (not forwarded) → gate doesn't apply.
	if err := proxySvc(false, false, false).authorizeSelfApply(ctx, "vendor"); err != nil {
		t.Fatalf("direct self-apply should pass, got %v", err)
	}
}

func TestProxyForwarded_OperatorGate(t *testing.T) {
	ctx := context.Background()

	// Forwarded + may-operate OFF (default) → denied even when may-apply is on.
	err := proxySvc(true, true, false).authorizeOperator(ctx)
	if err == nil || !strings.Contains(err.Error(), "trusted front proxy") {
		t.Fatalf("forwarded operator with may-operate off must be denied, got %v", err)
	}

	// Forwarded + may-operate ON → allowed.
	if err := proxySvc(true, true, true).authorizeOperator(ctx); err != nil {
		t.Fatalf("forwarded operator with may-operate on should pass, got %v", err)
	}

	// Direct → normal operator path.
	if err := proxySvc(false, true, false).authorizeOperator(ctx); err != nil {
		t.Fatalf("direct operator should pass, got %v", err)
	}
}

// A nil ProxyForwardedFromContext (mode off) must never deny.
func TestProxyForwarded_ModeOff_NeverDenies(t *testing.T) {
	s := New(nil, Config{
		AllowApplyMutation: true,
		CallerFromContext:  func(context.Context) string { return "vendor" },
		// ProxyForwardedFromContext left nil → mode off.
	})
	if err := s.authorizeSelfApply(context.Background(), "vendor"); err != nil {
		t.Fatalf("mode-off self-apply should pass, got %v", err)
	}
	if err := s.authorizeOperator(context.Background()); err != nil {
		t.Fatalf("mode-off operator should pass, got %v", err)
	}
}
