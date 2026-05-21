// Package runtime is the dependency surface for generated server code.
// Concrete impls: Pool/Tx/Batch in internal/storage/pg; Cache in internal/cache;
// Outbox in internal/cache/invalidate.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Pool is the minimum the generated handlers need from a pgx pool wrapper.
// Concrete implementation lives in internal/storage/pg/pool.go.
type Pool interface {
	// QueryRow runs a query expected to return at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) Row
	// Query runs a query that returns zero or more rows.
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	// Exec runs a statement that does not return rows.
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	// BeginTx starts a transaction. The handler is expected to call
	// Commit or Rollback on the returned Tx exactly once.
	BeginTx(ctx context.Context) (Tx, error)
}

// Tx is a database transaction. Mirrors the subset of pgx.Tx the codegen needs.
type Tx interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Row is one queried row. Generated code calls Scan to read columns out.
type Row interface {
	Scan(dest ...any) error
}

// Rows is a forward-only cursor over a result set.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// CommandTag carries the row count produced by a write.
type CommandTag interface {
	RowsAffected() int64
}

// Cache is the entity cache contract used by generated handlers. A body
// is keyed by `entity:id:ver` and a pointer key holds the current `ver`.
// The handler asks the cache for a body using a fixed encoded key; the
// cache handles pointer indirection internally.
type Cache interface {
	// Get returns the cached bytes for the given key, or ErrCacheMiss.
	// Implementations are expected to do tier-0 LRU → memcached → singleflight
	// internally; the handler never sees that complexity.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores bytes under key with the given TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// CurrentVersion returns the monotonic version pointer for an entity row,
	// or 0 if no pointer exists yet (caller treats as a miss).
	CurrentVersion(ctx context.Context, entity, id string) (int64, error)
}

// ErrCacheMiss is returned by Cache.Get when the key isn't present.
// Generated code uses errors.Is to detect this.
var ErrCacheMiss = errors.New("cache miss")

// ErrNotFound is returned by generated Get / Update / Delete handlers when
// the targeted row does not exist. The gRPC layer maps this to
// codes.NotFound; treating it as a normal error type (instead of a string
// or sentinel from pgx) keeps the handler-side imports minimal.
var ErrNotFound = errors.New("atlantis: not found")

// IsNoRows returns true when err means "the SELECT matched zero rows."
// Generated handlers call this to translate pgx-specific errors into our
// own ErrNotFound without importing pgx themselves. The Pool / Tx
// implementations are responsible for wrapping their no-row condition so
// errors.Is(err, sql.ErrNoRows) — or the pgx equivalent — is detectable.
//
// We accept "any error message containing 'no rows'" as a pragmatic fallback
// because pgx's sentinel name has changed across major versions and our
// generated code shouldn't care.
func IsNoRows(err error) bool {
	if err == nil {
		return false
	}
	// pgx.ErrNoRows.Error() is "no rows in result set". Check both forms
	// without importing pgx into the runtime package.
	msg := err.Error()
	return errors.Is(err, errNoRowsSentinel) ||
		msg == "no rows in result set" ||
		msg == "sql: no rows in result set"
}

// errNoRowsSentinel is the runtime's own sentinel; pool adapters can wrap
// their driver-specific no-rows error to point at this so errors.Is works.
var errNoRowsSentinel = errors.New("no rows")

// Outbox is how a write transaction records the cache invalidations it owes.
// The actual invalidation happens after commit via a LISTEN/NOTIFY worker.
// Generated handlers call Enqueue inside the tx before commit; everything
// after that is the runtime's problem.
type Outbox interface {
	// Enqueue records one invalidation. entity / id identify the row; the
	// new monotonic version is the new pointer value. tx is the open
	// transaction this enqueue must run inside (atomic with the write).
	Enqueue(ctx context.Context, tx Tx, entity, id string, newVersion int64) error

	// EnqueueGenerationBump records a per-entity tier-2 cache invalidation.
	// Generated Create/Update/Delete handlers call this after the existing
	// Enqueue so the worker eventually bumps the per-entity generation
	// counter, retiring every cached QueryX result for the entity at once.
	// Atomic with the write tx — same guarantees as Enqueue.
	EnqueueGenerationBump(ctx context.Context, tx Tx, entity string) error
}

// Deadline returns ctx with the per-RPC query deadline applied. If the
// caller's context already has a sooner deadline, that wins.
func Deadline(ctx context.Context, defaultMS int) (context.Context, context.CancelFunc) {
	if defaultMS <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(defaultMS)*time.Millisecond)
}

// CacheKey produces the atlantis body key for an entity row at
// a given version. Kept here (not the cache package) so the codegen has
// exactly one source for the format. Format: `atl:v1:{entity}:{id}:{ver}`.
func CacheKey(entity, id string, version int64) string {
	return "atl:v1:" + entity + ":" + id + ":" + itoa(version)
}

// PointerKey produces the version-pointer key. Format:
// `atl:v1:{entity}:{id}:ver`. The Cache implementation reads this to discover
// the current version, then fetches CacheKey(entity, id, ver).
func PointerKey(entity, id string) string {
	return "atl:v1:" + entity + ":" + id + ":ver"
}

// GenerationKey produces the per-entity tier-2 generation-counter key.
// Format: `atl:v2:{entity}:gen`. Every Create/Update/Delete eventually
// bumps this counter (via the outbox), invalidating every cached
// QueryX result for the entity at once.
func GenerationKey(entity string) string {
	return "atl:v2:" + entity + ":gen"
}

// QueryResultKey produces the tier-2 query-result cache key. Format:
// `atl:v2:{entity}:q:{hash}`. `hash` is the 128-bit digest
// derived in internal/cache/queryresult/hash.go (sha256 over canonical
// filter|order|limit|page_token|fields|includes|generation, truncated
// to 16 bytes and base64url-encoded).
func QueryResultKey(entity, hash string) string {
	return "atl:v2:" + entity + ":q:" + hash
}

// EncodeKeyArg renders one argument for an index-lookup cache key in a
// length-prefixed form so values containing the ":" separator can't make
// two distinct (arg list, value) pairs collide.
//
// Used by the cache-key codegen in internal/codegen/cache.go. Lives in the
// runtime package (rather than the generated keys package) so a single
// source defines the encoding for both writer and reader.
//
// Format: "<len>:<fmt.Sprint(v)>". time.Time values format via the
// reference RFC3339Nano output to avoid Go's location-dependent Sprint
// quirks; floats use %g for a canonical representation.
func EncodeKeyArg(v any) string {
	s := sprintCanonical(v)
	return itoa(int64(len(s))) + ":" + s
}

// CompositeID renders a composite primary key into the single string that
// the Outbox and Cache treat as the row identifier. Each component is
// length-prefixed (same encoding as EncodeKeyArg) and joined with "|" so
// no value can span boundaries — `("ab|cd", "ef")` and `("ab", "cd|ef")`
// produce distinct strings.
//
// Single-PK callers pass `CompositeID(id)`; composite-PK callers pass each
// PK column in DSL declaration order. The cache pointer / body key derives
// from the result via PointerKey / CacheKey unchanged.
func CompositeID(parts ...any) string {
	if len(parts) == 1 {
		return EncodeKeyArg(parts[0])
	}
	var b []byte
	for i, p := range parts {
		if i > 0 {
			b = append(b, '|')
		}
		b = append(b, EncodeKeyArg(p)...)
	}
	return string(b)
}

// sprintCanonical produces a stable string representation regardless of the
// runtime's local timezone or whether time.Time carries a monotonic clock
// reading.
func sprintCanonical(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	return sprintFallback(v)
}

// sprintFallback is fmt.Sprint, isolated so unit tests can sanity-check
// the canonical-vs-locale-dependent split.
func sprintFallback(v any) string { return fmt.Sprint(v) }

// itoa avoids the strconv import in every generated file. Keeping it private
// is fine; the codegen always reaches for the named helpers above.
func itoa(n int64) string {
	if n == 0 {
		return "0"
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
	return string(buf[i:])
}
