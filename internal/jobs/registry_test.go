package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register("vendor.ShopifyImport", HandlerFunc(func(ctx context.Context, args []byte) error {
		called = true
		return nil
	}))
	h := r.Lookup("vendor.ShopifyImport")
	if h == nil {
		t.Fatalf("expected handler to be registered")
	}
	if err := h.Handle(context.Background(), nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !called {
		t.Errorf("expected handler to run")
	}
}

func TestRegistry_LookupMissing(t *testing.T) {
	r := NewRegistry()
	if r.Lookup("vendor.Nope") != nil {
		t.Fatalf("expected nil for unregistered id")
	}
}

func TestRegistry_RegisterOverwrites(t *testing.T) {
	r := NewRegistry()
	r.Register("vendor.X", HandlerFunc(func(ctx context.Context, args []byte) error { return errors.New("first") }))
	r.Register("vendor.X", HandlerFunc(func(ctx context.Context, args []byte) error { return errors.New("second") }))
	err := r.Lookup("vendor.X").Handle(context.Background(), nil)
	if err == nil || err.Error() != "second" {
		t.Fatalf("expected second handler to win, got %v", err)
	}
}

func TestRegistry_RegisteredIDs(t *testing.T) {
	r := NewRegistry()
	r.Register("v.A", HandlerFunc(noopHandler))
	r.Register("v.B", HandlerFunc(noopHandler))
	r.Register("c.A", HandlerFunc(noopHandler))
	ids := r.RegisteredIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 ids, got %d: %v", len(ids), ids)
	}
}

func TestRegistry_ConcurrentRegisterLookup(t *testing.T) {
	// Confirms the mutex protects the map against a goroutine
	// registering while another reads. The race detector flags any
	// unprotected access; the assert here is secondary.
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Register("v.X", HandlerFunc(noopHandler))
		}()
		go func() {
			defer wg.Done()
			_ = r.Lookup("v.X")
		}()
	}
	wg.Wait()
}

func TestHandlerNotRegisteredError(t *testing.T) {
	err := &HandlerNotRegisteredError{JobID: "v.X"}
	if err.Error() == "" {
		t.Fatalf("error string should be non-empty")
	}
}

func TestDefaultConfig_Populated(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Schema != "atlantis" {
		t.Errorf("schema = %q", cfg.Schema)
	}
	if cfg.PodID == "" {
		t.Errorf("pod id is empty")
	}
	if cfg.BatchSize <= 0 {
		t.Errorf("batch size = %d", cfg.BatchSize)
	}
	if cfg.DrainInterval <= 0 {
		t.Errorf("drain interval = %v", cfg.DrainInterval)
	}
	if cfg.HeartbeatBudget <= 0 {
		t.Errorf("heartbeat budget = %v", cfg.HeartbeatBudget)
	}
}

func noopHandler(ctx context.Context, args []byte) error { return nil }
