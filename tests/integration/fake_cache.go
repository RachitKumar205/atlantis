//go:build integration

package integration

import (
	"context"
	"sync"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// FakeCache is an in-memory runtime.Cache for tests that drive handler logic
// without needing memcached's semantics. It satisfies the interface the
// generated server expects, so the miss/set sequence can be asserted cheaply.
//
// It is not a substitute for the testcontainers memcached in a test about
// invalidation timing, which depends on real SET ordering; Harness wires the
// Grafana fork for that. This is for:
//
//	cache := &FakeCache{}
//	s := NewAccountServer(harness.Pool, cache, harness.Outbox)
//	// ... drive s, then inspect cache.SetCalls
//
// Every public method takes a write lock. A test holds single-digit key counts,
// so nothing finer is worth the complexity.
type FakeCache struct {
	mu       sync.Mutex
	store    map[string]fakeEntry
	versions map[string]int64

	// SetCalls / GetCalls let tests assert "the handler hit the cache N
	// times" without instrumenting the handler.
	SetCalls int
	GetCalls int
}

type fakeEntry struct {
	value     []byte
	expiresAt time.Time
}

func NewFakeCache() *FakeCache {
	return &FakeCache{
		store:    map[string]fakeEntry{},
		versions: map[string]int64{},
	}
}

func (c *FakeCache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.GetCalls++
	e, ok := c.store[key]
	if !ok {
		return nil, runtime.ErrCacheMiss
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		delete(c.store, key)
		return nil, runtime.ErrCacheMiss
	}
	return append([]byte(nil), e.value...), nil
}

func (c *FakeCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.SetCalls++
	exp := time.Time{}
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	c.store[key] = fakeEntry{value: append([]byte(nil), value...), expiresAt: exp}
	return nil
}

func (c *FakeCache) CurrentVersion(ctx context.Context, entity, id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.versions[entity+":"+id], nil
}

// BumpVersion is a test-only helper that simulates the outbox worker's
// SetVersion call. Lets tests drive the cache through version transitions
// without standing up the real worker.
func (c *FakeCache) BumpVersion(entity, id string, v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versions[entity+":"+id] = v
}

// Reset clears all state and counters. Call between sub-tests in a table-
// driven test that reuses one FakeCache.
func (c *FakeCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = map[string]fakeEntry{}
	c.versions = map[string]int64{}
	c.SetCalls = 0
	c.GetCalls = 0
}

// Compile-time check that the fake satisfies the contract.
var _ runtime.Cache = (*FakeCache)(nil)
