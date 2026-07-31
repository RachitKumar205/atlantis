package entity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The read cache was built, wired into the invalidation worker, and never read
// from. internal/cache is ~1,800 lines of finished code; cmd/server constructed
// the Reader at main.go:146, handed it to the worker at :161 so invalidations
// would reach its LRU, and then constructed the entity server without it. Every
// Get went straight to Postgres.
//
// A required constructor argument does not prevent that recurring, and it is
// important to be honest about why: queryCache has been a required argument
// since this server was written, sits in the same struct, and is equally never
// read. Shape guarantees nothing. Only an assertion that a second Get does not
// reach the database can keep the read path wired.
//
// So this test counts loader invocations. Two Gets for the same row must load
// once.

type countingCache struct {
	mu       sync.Mutex
	versions map[string]int64
	bodies   map[string][]byte
}

func newCountingCache() *countingCache {
	return &countingCache{versions: map[string]int64{}, bodies: map[string][]byte{}}
}

func (c *countingCache) CurrentVersion(_ context.Context, entity, id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.versions[entity+"|"+id], nil
}

func (c *countingCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.bodies[key]
	if !ok {
		return nil, runtime.ErrCacheMiss
	}
	return b, nil
}

func (c *countingCache) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies[key] = value
	return nil
}

// A second Get for the same row must not reach the loader.
//
// Mutation that this catches and a shape check does not: delete the
// `if s.reader == nil` branch's else-arm in handleGet so every read goes
// straight to loadRow. The server still constructs, the argument is still
// required, every other test still passes, and the cache is dead again.
func TestReaderServesTheSecondCallWithoutReloading(t *testing.T) {
	cache := newCountingCache()
	reader, err := read.New(cache, read.Config{LRUSize: 16, DefaultTTL: time.Minute})
	if err != nil {
		t.Fatalf("read.New: %v", err)
	}

	var loads int
	loader := func(context.Context) ([]byte, error) {
		loads++
		return []byte("row-body"), nil
	}

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		body, err := reader.Get(ctx, "shop.Order", "1", loader)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if string(body) != "row-body" {
			t.Fatalf("Get %d returned %q", i, body)
		}
	}
	if loads != 1 {
		t.Errorf("loader ran %d times for two Gets of the same row, want 1 — the "+
			"read path is not consulting the cache", loads)
	}
}

// And invalidation must actually put it back in the loading state, or the
// cache would serve a row forever. This is the property the whole outbox and
// worker pipeline exists to deliver; without it, wiring the read path would
// turn a working system into one that serves stale data.
func TestInvalidationForcesAReload(t *testing.T) {
	cache := newCountingCache()
	reader, err := read.New(cache, read.Config{LRUSize: 16, DefaultTTL: time.Minute})
	if err != nil {
		t.Fatalf("read.New: %v", err)
	}

	var loads int
	body := "v1"
	loader := func(context.Context) ([]byte, error) {
		loads++
		return []byte(body), nil
	}

	ctx := context.Background()
	if _, err := reader.Get(ctx, "shop.Order", "1", loader); err != nil {
		t.Fatal(err)
	}

	// What the invalidation worker does for every drained outbox row: bump the
	// version in the pointer cache, then drop the in-process LRU entry.
	cache.mu.Lock()
	cache.versions["shop.Order|1"]++
	cache.mu.Unlock()
	reader.Invalidate("shop.Order", "1")

	body = "v2"
	got, err := reader.Get(ctx, "shop.Order", "1", loader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v2" {
		t.Errorf("after invalidation Get returned %q, want v2 — a stale row is "+
			"being served, which is the failure the read cache must not "+
			"introduce", got)
	}
	if loads != 2 {
		t.Errorf("loader ran %d times, want 2 — invalidation did not return the "+
			"row to the loading state", loads)
	}
}
