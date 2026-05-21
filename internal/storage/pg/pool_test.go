package pg

import (
	"context"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

func TestDefaultConfig_MatchesPlan(t *testing.T) {
	c := DefaultConfig("postgres://example")
	// Numbers from PLAN.md §B.5. They are tuned per-deployment, but the
	// defaults committed here are the starting point.
	if c.MaxConns != 50 {
		t.Errorf("MaxConns: got %d want 50", c.MaxConns)
	}
	if c.MinConns != 10 {
		t.Errorf("MinConns: got %d want 10", c.MinConns)
	}
	if c.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("MaxConnIdleTime: got %v want 5m", c.MaxConnIdleTime)
	}
	if c.MaxConnLifetime != time.Hour {
		t.Errorf("MaxConnLifetime: got %v want 1h", c.MaxConnLifetime)
	}
	if c.HealthCheckPeriod != 30*time.Second {
		t.Errorf("HealthCheckPeriod: got %v want 30s", c.HealthCheckPeriod)
	}
	if c.QueryTimeoutDefault != 2*time.Second {
		t.Errorf("QueryTimeoutDefault: got %v want 2s", c.QueryTimeoutDefault)
	}
}

func TestNew_RejectsEmptyURL(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatalf("expected error on empty URL")
	}
}

func TestNew_RejectsBadURL(t *testing.T) {
	if _, err := New(context.Background(), Config{URL: "not-a-url"}); err == nil {
		t.Fatalf("expected error on bad URL")
	}
}

func TestPool_SatisfiesRuntimeInterface(t *testing.T) {
	// Compile-time check is in pool.go; this test just documents the
	// contract. If a future refactor breaks it, this test pins the
	// regression to one spot.
	var _ runtime.Pool = (*Pool)(nil)
}
