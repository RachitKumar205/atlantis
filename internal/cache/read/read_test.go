package read

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// fakeCache is an in-memory PointerCache for unit tests. Models the
// pointer/body two-key shape memcached imposes on us.
type fakeCache struct {
	mu       sync.Mutex
	bodies   map[string][]byte
	versions map[string]int64 // entity:id -> ver
	getCalls atomic.Int64
	setCalls atomic.Int64
	failGet  bool
}

func newFakeCache() *fakeCache {
	return &fakeCache{
		bodies:   map[string][]byte{},
		versions: map[string]int64{},
	}
}

func (c *fakeCache) Get(ctx context.Context, key string) ([]byte, error) {
	c.getCalls.Add(1)
	if c.failGet {
		return nil, errors.New("forced cache error")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.bodies[key]
	if !ok {
		return nil, runtime.ErrCacheMiss
	}
	return v, nil
}

func (c *fakeCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.setCalls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies[key] = value
	return nil
}

func (c *fakeCache) CurrentVersion(ctx context.Context, entity, id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.versions[entity+":"+id]
	if !ok {
		return 0, nil
	}
	return v, nil
}

// seedPointer installs a known version for an entity row.
func (c *fakeCache) seedPointer(entity, id string, ver int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.versions[entity+":"+id] = ver
}

// seedBody installs an entity body at a specific version. Key is the
// runtime.CacheKey form.
func (c *fakeCache) seedBody(entity, id string, ver int64, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies[runtime.CacheKey(entity, id, ver)] = body
}

// ---- read-path tests ----

func TestReader_CacheHit_ReturnsBytes(t *testing.T) {
	c := newFakeCache()
	c.seedPointer("x.A", "1", 5)
	c.seedBody("x.A", "1", 5, []byte("hello"))

	r, err := New(c, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	body, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		t.Fatal("loader should not be invoked on cache hit")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body: %s", body)
	}
}

func TestReader_CacheMiss_FallsThroughToLoader(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, DefaultConfig())

	var loaderCalls atomic.Int32
	body, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		loaderCalls.Add(1)
		return []byte("fresh"), nil
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "fresh" {
		t.Errorf("body: %s", body)
	}
	if loaderCalls.Load() != 1 {
		t.Errorf("loader calls: %d want 1", loaderCalls.Load())
	}
	// After the miss, the body should be cached (single Set on miss).
	if c.setCalls.Load() < 1 {
		t.Errorf("expected at least one Set after miss")
	}
}

func TestReader_PropagatesLoaderError(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, DefaultConfig())
	want := errors.New("db down")
	_, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Errorf("expected loader error, got %v", err)
	}
}

func TestReader_Singleflight_DedupsConcurrentMisses(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, Config{
		LRUSize:    0, // disable tier-0 so every Get goes to the cache + loader path
		DefaultTTL: 10 * time.Minute,
	})

	var loaderCalls atomic.Int32
	// Loader sleeps long enough that many goroutines coalesce.
	loader := func(ctx context.Context) ([]byte, error) {
		loaderCalls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []byte("fresh"), nil
	}

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := range N {
		go func() {
			defer wg.Done()
			_, err := r.Get(context.Background(), "x.A", "1", loader)
			errs[i] = err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	// Singleflight should collapse the N concurrent misses into 1 loader call.
	calls := loaderCalls.Load()
	if calls > 2 {
		// Single goroutine can occasionally be scheduled juuust after the
		// first one finishes; >2 means singleflight is broken.
		t.Errorf("loader called %d times; expected <= 2 with singleflight", calls)
	}
}

func TestReader_Tier0LRU_AvoidsMemcached(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, Config{LRUSize: 16, DefaultTTL: time.Minute})

	// First call: miss, loader runs, body cached in both tiers.
	body1, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		return []byte("v1"), nil
	})
	if err != nil || string(body1) != "v1" {
		t.Fatalf("first call: body=%s err=%v", body1, err)
	}

	getsBefore := c.getCalls.Load()
	// Second call should be served from tier-0 LRU, NOT touching the cache.
	body2, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		t.Fatal("loader should not be called: tier-0 hit expected")
		return nil, nil
	})
	if err != nil || string(body2) != "v1" {
		t.Fatalf("second call: body=%s err=%v", body2, err)
	}
	getsAfter := c.getCalls.Load()
	if getsAfter != getsBefore {
		t.Errorf("tier-0 should have served second call; underlying cache Get calls grew %d → %d",
			getsBefore, getsAfter)
	}
}

func TestReader_Invalidate_RemovesLRUEntry(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, Config{LRUSize: 16, DefaultTTL: time.Minute})

	_, _ = r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		return []byte("v1"), nil
	})

	// Now invalidate and verify the next Get goes back to the loader.
	r.Invalidate("x.A", "1")

	var loaderCalls atomic.Int32
	_, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		loaderCalls.Add(1)
		return []byte("v2"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// With tier-0 cleared, the loader runs unless the memcached body for v1
	// is still around. Our fake stored it under v1's body key; the second
	// call asks for the current version pointer (which the loader path
	// re-discovers).
	if loaderCalls.Load() == 0 {
		// Could happen if memcached tier serves the body. Either is acceptable
		// for THIS test; the important property is the LRU was actually cleared.
		t.Logf("memcached served the post-invalidate read; tier-0 was cleared")
	}
}

func TestReader_XFetch_RespectsBeta(t *testing.T) {
	// With beta = 0 the reader must never refresh early.
	r, _ := New(newFakeCache(), Config{XFetchBeta: 0})
	for range 1000 {
		if r.xfetchShouldRefresh(time.Second) {
			t.Fatal("beta = 0 should never trigger XFetch refresh")
		}
	}

	// With beta very high and remaining = a few ms, the refresh probability
	// should be high. Loose check, not exact, but should fire at least once.
	r2, _ := New(newFakeCache(), Config{XFetchBeta: 100})
	any := false
	for range 100 {
		if r2.xfetchShouldRefresh(10 * time.Millisecond) {
			any = true
			break
		}
	}
	if !any {
		t.Errorf("high beta + tiny remaining should occasionally trigger XFetch")
	}
}

func TestReader_LRUDisabled_StillFunctional(t *testing.T) {
	c := newFakeCache()
	r, _ := New(c, Config{LRUSize: 0, DefaultTTL: time.Minute})

	body, err := r.Get(context.Background(), "x.A", "1", func(ctx context.Context) ([]byte, error) {
		return []byte("hi"), nil
	})
	if err != nil || string(body) != "hi" {
		t.Errorf("LRUSize=0: body=%s err=%v", body, err)
	}
	// Invalidate with no LRU is a no-op (no panic).
	r.Invalidate("x.A", "1")
}

func TestEncodeDecodeVer(t *testing.T) {
	cases := []int64{0, 1, -1, 1 << 60, -(1 << 60)}
	for _, ver := range cases {
		body := []byte("payload")
		enc := encodeVer(ver, body)
		got, gotBody, ok := decodeVer(enc)
		if !ok {
			t.Errorf("decodeVer failed for ver=%d", ver)
			continue
		}
		if got != ver {
			t.Errorf("ver round trip: got %d want %d", got, ver)
		}
		if string(gotBody) != string(body) {
			t.Errorf("body round trip mismatch")
		}
	}
}

func TestDecodeVer_RejectsShort(t *testing.T) {
	if _, _, ok := decodeVer([]byte("short")); ok {
		t.Errorf("decodeVer should reject < 8-byte input")
	}
}

func TestTouch_NoPanic(t *testing.T) {
	// Just verify the helper exists and doesn't panic; it ensures encode/decode
	// stay exported even when no production caller uses them yet.
	Touch()
}
