// Package memcached wraps grafana/gomemcache with context-aware ops
// and runtime.ErrCacheMiss translation.
package memcached

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/grafana/gomemcache/memcache"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Config tunes the underlying gomemcache.Client. Defaults are conservative;
// production overrides come from env vars in cmd/server.
type Config struct {
	// Addrs is one or more "host:port" memcached endpoints. Consistent
	// hashing across endpoints is handled by the client.
	Addrs []string

	// Timeout for individual operations. Distinct from any per-RPC deadline
	// already on ctx — whichever is shorter wins.
	Timeout time.Duration

	// MaxIdleConns per server. Grafana's fork tunes this internally; we
	// surface it for parity with the upstream API.
	MaxIdleConns int
}

// DefaultConfig builds a Config from a list of memcached endpoints with
// default timeouts.
func DefaultConfig(addrs []string) Config {
	return Config{
		Addrs:        addrs,
		Timeout:      100 * time.Millisecond,
		MaxIdleConns: 8,
	}
}

// Client is a context-aware wrapper around the Grafana gomemcache client.
// It satisfies runtime.Cache (modulo the version pointer helper, which
// requires the read-path package's pointer-key convention).
type Client struct {
	inner *memcache.Client
}

// New validates cfg and returns a Client. Returns an error if cfg.Addrs is empty.
func New(cfg Config) (*Client, error) {
	if len(cfg.Addrs) == 0 {
		return nil, fmt.Errorf("memcached: no addresses configured")
	}
	c := memcache.New(cfg.Addrs...)
	if cfg.Timeout > 0 {
		c.Timeout = cfg.Timeout
	}
	if cfg.MaxIdleConns > 0 {
		c.MaxIdleConns = cfg.MaxIdleConns
	}
	return &Client{inner: c}, nil
}

// Close releases any pooled connections. The Grafana fork's Close returns no
// error; this signature keeps one so another driver can report a shutdown
// failure without changing the API.
func (c *Client) Close() error {
	c.inner.Close()
	return nil
}

// Get returns the bytes stored under key, or runtime.ErrCacheMiss.
//
// gomemcache takes no context, so ctx is honoured by checking for cancellation
// before the call. Config's per-op Timeout is the hard upper bound; a ctx
// deadline sooner than that returns an error the reader treats as any other
// failed lookup.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item, err := c.inner.Get(key)
	if err != nil {
		if errors.Is(err, memcache.ErrCacheMiss) {
			return nil, runtime.ErrCacheMiss
		}
		return nil, err
	}
	return item.Value, nil
}

// GetMulti reads multiple keys in one round trip. Missing keys are omitted
// from the result map; the caller treats absent entries as misses.
func (c *Client) GetMulti(ctx context.Context, keys []string) (map[string][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items, err := c.inner.GetMulti(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(items))
	for k, item := range items {
		out[k] = item.Value
	}
	return out, nil
}

// Set stores value under key with the given TTL. TTLs over 30 days are
// interpreted as absolute Unix timestamps by memcached itself; the wrapper
// caps anything past 30 days at exactly 30 days to avoid that pitfall.
func (c *Client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exp := ttlToExpiration(ttl)
	return c.inner.Set(&memcache.Item{
		Key:        key,
		Value:      value,
		Expiration: exp,
	})
}

// Delete removes key. Missing keys are treated as success — invalidation
// must be idempotent because the same outbox row may be processed twice if
// the worker crashes between memcached and the outbox-row DELETE.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := c.inner.Delete(key)
	if err != nil && !errors.Is(err, memcache.ErrCacheMiss) {
		return err
	}
	return nil
}

// CurrentVersion reads the version pointer key for an entity row. Returns 0
// on miss, which the read path treats as "fall through to PG". This method
// satisfies the runtime.Cache contract from the generated handlers' POV.
func (c *Client) CurrentVersion(ctx context.Context, entity, id string) (int64, error) {
	key := runtime.PointerKey(entity, id)
	val, err := c.Get(ctx, key)
	if err != nil {
		if errors.Is(err, runtime.ErrCacheMiss) {
			return 0, nil
		}
		return 0, err
	}
	v, err := parseInt64(val)
	if err != nil {
		return 0, fmt.Errorf("memcached: corrupt version pointer for %s/%s: %w", entity, id, err)
	}
	// Defense-in-depth: a memcached-network attacker who tampers with the
	// pointer key could submit a wildly large or negative value. Reject
	// implausible ranges. The legitimate version counter is
	// per-row monotonic; even a hot row taking 1 write per ms for a year
	// stays under 2^35.
	if v <= 0 || v > (1<<40) {
		return 0, fmt.Errorf("memcached: implausible version %d for %s/%s", v, entity, id)
	}
	return v, nil
}

// SetVersion stores the new version under the pointer key with the given
// TTL. Used by the invalidation outbox worker.
//
// Monotonic: refuses a version at or below the current pointer. Without the
// guard, two workers processing rows for one entity can race and leave the
// pointer at the lower version.
//
// Read-then-write rather than memcached CAS, because the pointer key may not
// exist on first write and CAS needs an existing value.
//
// Three return shapes:
//
//   - nil — version accepted and written.
//   - ErrStaleVersion — the cached pointer is already at-or-ahead of
//     `version`. The worker treats this as success-equivalent (the desired
//     state is in cache already) and DELETEs the outbox row.
//   - any other error — CurrentVersion failed, so the guard is unvalidated, or
//     the write itself failed. The worker retries, leaving the outbox row for
//     the next tick. The read error is returned rather than swallowed: SET on
//     an unvalidated guard lets a delayed worker overwrite a fresher pointer
//     another worker published since this one last read.
func (c *Client) SetVersion(ctx context.Context, entity, id string, version int64, ttl time.Duration) error {
	if version <= 0 {
		return fmt.Errorf("memcached: refusing to write non-positive version %d for %s/%s", version, entity, id)
	}
	cur, err := c.CurrentVersion(ctx, entity, id)
	if err != nil {
		// Read failure: the monotonic guard can't be validated. Refuse
		// the write — see the doc-comment above for why a "fresh pointer
		// better than stuck cache" fallback is the wrong call here.
		return fmt.Errorf("memcached: monotonic guard read failed for %s/%s: %w", entity, id, err)
	}
	if cur >= version {
		return ErrStaleVersion
	}
	return c.Set(ctx, runtime.PointerKey(entity, id), formatInt64(version), ttl)
}

// ErrStaleVersion is returned by SetVersion when the new version is not
// strictly greater than the cached pointer. Callers (the worker) treat it
// as a non-error — the row's version is already at-or-ahead in cache.
var ErrStaleVersion = errors.New("memcached: stale version pointer write rejected")

// IsStale implements the worker's VersionSetter.IsStale check. We don't
// export `ErrStaleVersion` across the package boundary so the worker
// doesn't import this package; instead, this method does the check.
func (c *Client) IsStale(err error) bool { return errors.Is(err, ErrStaleVersion) }

// Increment atomically increments an unsigned counter at key by delta. Against
// a missing key it seeds to delta through a fallback SET, since memcached's
// `incr` errors on one.
//
// The tier-2 query-result cache keeps its per-entity generation counters this
// way: each Create, Update and Delete enqueues a generation_bump, and the
// worker calls Increment when it drains the row. The worker's debouncing keeps
// a write burst from becoming one Increment per write.
func (c *Client) Increment(ctx context.Context, key string, delta uint64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.inner.Increment(key, delta)
	if err == nil {
		// gomemcache's Increment returns uint64; the result fits in int64
		// in practice (generation counters won't pass 2^63 in any plausible
		// world). Treat overflow defensively.
		if n > (1 << 62) {
			return 0, fmt.Errorf("memcached: implausible counter %d at %q", n, key)
		}
		return int64(n), nil
	}
	if !errors.Is(err, memcache.ErrCacheMiss) {
		return 0, err
	}
	// Seed: memcached.Increment doesn't auto-create. We add the key with
	// the value `delta` (encoded as decimal-string, like SetVersion). If
	// two concurrent writers race here the second's Add fails; we fall
	// through to a second Increment which now sees the seeded value.
	if err := c.inner.Add(&memcache.Item{
		Key:        key,
		Value:      formatInt64(int64(delta)),
		Expiration: ttlToExpiration(thirtyDays),
	}); err == nil {
		return int64(delta), nil
	} else if !errors.Is(err, memcache.ErrNotStored) {
		return 0, err
	}
	// Lost the seed race; another writer landed first. Retry once.
	n, err = c.inner.Increment(key, delta)
	if err != nil {
		return 0, err
	}
	if n > (1 << 62) {
		return 0, fmt.Errorf("memcached: implausible counter %d at %q", n, key)
	}
	return int64(n), nil
}

// CounterValue reads the integer value at key. Returns 0 for a missing
// key (treated as "no generation bumps recorded yet"). Used by tier-2
// readers to fold the generation counter into the cache key hash.
func (c *Client) CounterValue(ctx context.Context, key string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	item, err := c.inner.Get(key)
	if err != nil {
		if errors.Is(err, memcache.ErrCacheMiss) {
			return 0, nil
		}
		return 0, err
	}
	v, err := parseInt64(item.Value)
	if err != nil {
		return 0, fmt.Errorf("memcached: counter %q: %w", key, err)
	}
	if v < 0 || v > (1<<62) {
		return 0, fmt.Errorf("memcached: implausible counter %d at %q", v, key)
	}
	return v, nil
}

const thirtyDays = 30 * 24 * time.Hour

func ttlToExpiration(ttl time.Duration) int32 {
	if ttl <= 0 {
		return 0 // never expires
	}
	if ttl > thirtyDays {
		ttl = thirtyDays
	}
	return int32(ttl / time.Second)
}

// parseInt64 and formatInt64 pin the on-wire encoding of version pointers, so
// the worker and the reader read the same bytes the same way.
//
// strconv.ParseInt rejects overflow with ErrRange — a string past 19 digits, or
// a number above max int64. A hand-rolled loop wraps silently, which anything
// on the memcached network can use to point the reader at a version it chose.
func parseInt64(b []byte) (int64, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("empty")
	}
	return strconv.ParseInt(string(b), 10, 64)
}

func formatInt64(n int64) []byte {
	if n == 0 {
		return []byte("0")
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return slices.Clone(buf[i:])
}

// Compile-time check: *Client provides the cache interface the generated
// server consumes (runtime.Cache).
var _ runtime.Cache = (*Client)(nil)
