package invalidate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// VersionSetter is the slice of the memcached client the worker needs.
// internal/cache/memcached.*Client satisfies this; the small interface
// keeps the worker package free of memcache-specific imports.
//
// IsStale lets the worker distinguish "we lost the race against another
// worker — row is done" from a real apply failure. The memcached client
// returns its ErrStaleVersion sentinel; this method wraps the check so the
// worker doesn't need to import that sentinel directly.
type VersionSetter interface {
	SetVersion(ctx context.Context, entity, id string, version int64, ttl time.Duration) error
	IsStale(err error) bool
}

// LRUInvalidator is the in-process LRU notification surface. The reader
// package's *Reader satisfies this. After memcached SET succeeds we tell
// every running reader to drop its tier-0 entry; without that, tier-0
// would happily keep serving stale bytes until eviction.
type LRUInvalidator interface {
	Invalidate(entity, id string)
}

// GenerationBumper increments the per-entity counter that keys the
// tier-2 query-result cache. queryresult.*Cache satisfies this; the
// narrow interface keeps the invalidate package decoupled from the
// query-result cache implementation.
//
// One bump invalidates every cached query result for the entity at
// once. Bursts of writes get coalesced by the worker's per-entity
// debouncer so memcached doesn't see one counter increment per row.
type GenerationBumper interface {
	BumpGeneration(ctx context.Context, entity string) (int64, error)
}

// WorkerConfig tunes the drain loop.
type WorkerConfig struct {
	// Schema overrides the default "atlantis" schema name.
	Schema string

	// DrainInterval is the periodic wake-up interval; the worker also wakes
	// on LISTEN/NOTIFY. Default 250ms.
	DrainInterval time.Duration

	// BatchSize is the max rows drained per loop. Default 100.
	BatchSize int

	// PointerTTL is the TTL on the version-pointer key when SET in memcached.
	// Long enough that an offline reader cluster catches up after restart.
	// Default 24h.
	PointerTTL time.Duration

	// AlertLag is the threshold over which the sweeper logs at WARN that the
	// worker is behind. Default 5m.
	AlertLag time.Duration

	// BumpDebounce is the minimum gap between successive
	// BumpGeneration calls for the same entity. A burst of writes to
	// one entity within this window collapses to a single bump.
	// Default 100ms.
	BumpDebounce time.Duration

	// Logger receives structured events. Defaults to slog.Default.
	Logger *slog.Logger
}

// DefaultWorkerConfig returns the default defaults.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		Schema:        "atlantis",
		DrainInterval: 250 * time.Millisecond,
		BatchSize:     100,
		PointerTTL:    24 * time.Hour,
		AlertLag:      5 * time.Minute,
		BumpDebounce:  100 * time.Millisecond,
	}
}

// Worker drains the cache_invalidations outbox.
//
// The drain loop:
//  1. Wake on LISTEN/NOTIFY ("atl_cache_invalidations") OR periodic timer.
//  2. SELECT the oldest batch of rows.
//  3. For each row: SetVersion in memcached, Invalidate the in-process LRU,
//     DELETE the outbox row. Failures bump `attempts` and leave the row in
//     place for the next loop.
//
// Worker safety: there can be multiple workers in different pods. The
// SELECT uses FOR UPDATE SKIP LOCKED so two workers never claim the same
// row, and per-row success is idempotent (memcached SET is idempotent, the
// DELETE removes the row exactly once across the cluster).
type Worker struct {
	pool *pgxpool.Pool
	mc   VersionSetter
	lru  LRUInvalidator
	qc   GenerationBumper
	cfg  WorkerConfig

	// lastBump tracks the most recent BumpGeneration call per entity.
	// Subsequent generation_bump rows for the same entity within
	// cfg.BumpDebounce are consumed without re-bumping. The mutex is
	// cheap relative to memcached round-trips.
	bumpMu   sync.Mutex
	lastBump map[string]time.Time

	stopOnce sync.Once
	stopped  chan struct{}
}

// NewWorker constructs a Worker. The pool MUST be a pgxpool so we can use
// pgx's native LISTEN/NOTIFY via Acquire(). Returns an error iff
// cfg.Schema is not a valid SQL identifier — the worker's SQL is built
// with fmt.Sprintf so a hostile schema would otherwise produce injection.
//
// qc is optional. When nil, generation_bump rows are still drained but
// the bump is a no-op — this lets older callers (and tests that don't
// exercise tier-2 caching) keep working without wiring a query-cache.
func NewWorker(pool *pgxpool.Pool, mc VersionSetter, lru LRUInvalidator, qc GenerationBumper, cfg WorkerConfig) (*Worker, error) {
	if cfg.Schema == "" {
		cfg.Schema = "atlantis"
	}
	if err := validateSchema(cfg.Schema); err != nil {
		return nil, err
	}
	if cfg.DrainInterval == 0 {
		cfg.DrainInterval = 250 * time.Millisecond
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	if cfg.PointerTTL == 0 {
		cfg.PointerTTL = 24 * time.Hour
	}
	if cfg.AlertLag == 0 {
		cfg.AlertLag = 5 * time.Minute
	}
	if cfg.BumpDebounce == 0 {
		cfg.BumpDebounce = 100 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Worker{
		pool:     pool,
		mc:       mc,
		lru:      lru,
		qc:       qc,
		cfg:      cfg,
		lastBump: make(map[string]time.Time),
		stopped:  make(chan struct{}),
	}, nil
}

// Run blocks until ctx is canceled. Errors during drain are logged and
// retried; only fatal acquire / LISTEN errors propagate up.
//
// In production this runs in its own goroutine launched by cmd/server.
// In tests, run it in a goroutine and cancel ctx to stop.
func (w *Worker) Run(ctx context.Context) error {
	defer close(w.stopped)

	// Acquire a dedicated connection for LISTEN. This connection is held for
	// the worker's lifetime; the rest of the work uses the regular pool.
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("worker: acquire listen conn: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN atl_cache_invalidations"); err != nil {
		return fmt.Errorf("worker: LISTEN: %w", err)
	}

	// Run one drain pass immediately to clear anything that landed before
	// LISTEN was registered.
	w.drainOnce(ctx)

	ticker := time.NewTicker(w.cfg.DrainInterval)
	defer ticker.Stop()

	// notifyCh is a small buffered channel fed by the LISTEN goroutine. We
	// don't care about the payload — any notify means "wake up and drain".
	notifyCh := make(chan struct{}, 1)
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	go func() {
		for {
			if _, err := conn.Conn().WaitForNotification(listenCtx); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				w.cfg.Logger.Warn("listen: wait error", "err", err)
				// Brief backoff to avoid spinning if the connection drops.
				select {
				case <-listenCtx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			select {
			case notifyCh <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drainOnce(ctx)
		case <-notifyCh:
			w.drainOnce(ctx)
		}
	}
}

// drainOnce processes up to BatchSize rows, then returns. Per-row failures
// are logged and skipped; the row stays in the outbox for the next iter.
//
// Concurrency invariant: claim + apply + delete are wrapped in
// a single Postgres transaction. `SELECT ... FOR UPDATE SKIP LOCKED` holds
// the row lock until COMMIT — without the tx, the lock was released the
// moment the SELECT returned, allowing another worker to claim and process
// the same row and produce a stale pointer overwrite. With the tx, only
// one worker can hold a given outbox row at a time across the full cycle.
//
// We do memcached SET *inside* the tx (which means an open Postgres tx
// while making a network call to memcached). That's normally an anti-
// pattern (transactions should not wrap external IO) but here:
//
//   - the tx is the worker's tx, not a data-write tx — its only purpose
//     is to serialize outbox processing
//   - the memcached call has a short hard timeout (100ms by default)
//   - the tx holds a row lock on a small bounded set of cache_invalidations
//     rows; it does not block any data path
//
// So the cost is bounded and the alternative (releasing the lock then
// racing the DELETE) is worse.
func (w *Worker) drainOnce(ctx context.Context) {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		w.cfg.Logger.Warn("drain: begin", "err", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := w.claimInTx(ctx, tx)
	if err != nil {
		w.cfg.Logger.Warn("drain: claim", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	for _, r := range rows {
		if err := w.apply(ctx, r); err != nil {
			w.cfg.Logger.Warn("drain: apply",
				"id", r.ID, "kind", r.Kind, "entity", r.Entity, "row", r.RowID, "err", err)
			// Mark as failure inside the tx so the attempts counter goes up
			// even on retryable errors. The row stays — we did not DELETE.
			w.markFailureInTx(ctx, tx, r.ID, err)
			continue
		}
		// LRU drop only matters for row-level invalidations. Generation
		// bumps invalidate the tier-2 query cache, which is not tier-0
		// LRU's concern.
		if w.lru != nil && (r.Kind == "" || r.Kind == "invalidation") {
			w.lru.Invalidate(r.Entity, r.RowID)
		}
		if err := w.deleteInTx(ctx, tx, r.ID); err != nil {
			w.cfg.Logger.Warn("drain: delete row", "id", r.ID, "err", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		w.cfg.Logger.Warn("drain: commit", "err", err)
	}
}

type pendingRow struct {
	ID         int64
	Kind       string // "invalidation" or "generation_bump"
	Entity     string
	RowID      string
	NewVersion int64
	EnqueuedAt time.Time
	Attempts   int
}

// claimInTx pulls a batch of outbox rows inside the worker's transaction.
// `FOR UPDATE SKIP LOCKED` holds the row lock until the surrounding tx
// commits or rolls back — long enough for the DELETE that finishes the row.
func (w *Worker) claimInTx(ctx context.Context, tx pgx.Tx) ([]pendingRow, error) {
	q := fmt.Sprintf(`
SELECT id, kind, entity, row_id, new_version, enqueued_at, attempts
FROM %s.cache_invalidations
ORDER BY enqueued_at
LIMIT $1
FOR UPDATE SKIP LOCKED`, w.cfg.Schema)
	rows, err := tx.Query(ctx, q, w.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.Entity, &r.RowID, &r.NewVersion, &r.EnqueuedAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// apply dispatches a single outbox row by kind. The work it does is
// kind-specific, but the contract is the same: nil means the row was
// handled (the surrounding tx will DELETE it); a non-nil error means the
// row stays put and `attempts` increments.
//
// Lag alerting fires unconditionally because either kind of row stuck
// in the outbox indicates either the worker is wedged or memcached is
// down. We log here (not in the sweeper) so the first-time alert is
// immediate.
func (w *Worker) apply(ctx context.Context, r pendingRow) error {
	if age := time.Since(r.EnqueuedAt); age > w.cfg.AlertLag {
		w.cfg.Logger.Warn("outbox lag",
			"kind", r.Kind, "entity", r.Entity, "row", r.RowID, "age", age, "attempts", r.Attempts)
	}
	switch r.Kind {
	case "", "invalidation":
		// Empty string handles the (pre-migration / test) case where
		// the kind column hasn't been backfilled yet.
		return w.applyInvalidation(ctx, r)
	case "generation_bump":
		return w.applyGenerationBump(ctx, r)
	default:
		// Unknown kind = poisoned outbox row. Returning a non-nil
		// error keeps the row in place; the attempts counter will
		// climb until an operator intervenes. We deliberately don't
		// silently DELETE — corruption should be visible.
		return fmt.Errorf("unknown outbox kind %q", r.Kind)
	}
}

// applyInvalidation runs the memcached SET that publishes a row's new
// version pointer. The pointer TTL is long (default 24h); readers fall
// back to PG if they ever see no pointer.
//
// ErrStaleVersion (from the memcached client's monotonic guard) is NOT
// an error from the worker's perspective — it means another worker
// already landed a higher version. We delete the row and move on.
func (w *Worker) applyInvalidation(ctx context.Context, r pendingRow) error {
	err := w.mc.SetVersion(ctx, r.Entity, r.RowID, r.NewVersion, w.cfg.PointerTTL)
	if err != nil && w.mc.IsStale(err) {
		return nil
	}
	return err
}

// applyGenerationBump increments the per-entity counter that keys the
// tier-2 query-result cache, subject to per-entity debouncing.
//
// The debouncer collapses bursts of writes to one entity into a single
// counter increment. A burst of N writes within BumpDebounce produces
// one bump (covering all N) rather than N bumps; readers who issued a
// query result mid-burst may serve up to (TTL + BumpDebounce) stale
// state, which is well within the query-result cache's documented
// freshness model.
//
// When the worker was constructed without a GenerationBumper (older
// callers, tests that don't exercise tier-2), the bump is a no-op —
// the row still gets DELETEd so it doesn't pile up.
func (w *Worker) applyGenerationBump(ctx context.Context, r pendingRow) error {
	if w.qc == nil {
		return nil
	}
	if r.Entity == "" {
		return errors.New("generation_bump: empty entity")
	}

	w.bumpMu.Lock()
	last, seen := w.lastBump[r.Entity]
	if seen && time.Since(last) < w.cfg.BumpDebounce {
		w.bumpMu.Unlock()
		return nil
	}
	w.lastBump[r.Entity] = time.Now()
	w.bumpMu.Unlock()

	if _, err := w.qc.BumpGeneration(ctx, r.Entity); err != nil {
		// The bump failed; roll back our debounce record so a retry
		// isn't suppressed.
		w.bumpMu.Lock()
		if w.lastBump[r.Entity].Equal(last) || !seen {
			delete(w.lastBump, r.Entity)
		}
		w.bumpMu.Unlock()
		return err
	}
	return nil
}

// markFailureInTx bumps attempts and records last_error. Sanitized so we
// never persist raw error messages — those can leak hostnames, query
// fragments, or PII. We persist only the error *kind* and a truncated
// category.
func (w *Worker) markFailureInTx(ctx context.Context, tx pgx.Tx, id int64, applyErr error) {
	msg := sanitizeError(applyErr)
	q := fmt.Sprintf(`
UPDATE %s.cache_invalidations
SET attempts = attempts + 1, last_error = $2
WHERE id = $1`, w.cfg.Schema)
	if _, err := tx.Exec(ctx, q, id, msg); err != nil {
		w.cfg.Logger.Warn("mark failure", "id", id, "err", err)
	}
}

// deleteInTx removes a successfully-applied outbox row, inside the worker tx.
func (w *Worker) deleteInTx(ctx context.Context, tx pgx.Tx, id int64) error {
	q := fmt.Sprintf(`DELETE FROM %s.cache_invalidations WHERE id = $1`, w.cfg.Schema)
	_, err := tx.Exec(ctx, q, id)
	return err
}

// sanitizeError keeps a tiny category string so operators can grep, without
// persisting attacker-influenceable raw text. Categories pinned here so the
// audit story is "the only strings we ever store are these constants".
func sanitizeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	// Everything else is "apply_failed". We refuse to persist the raw
	// message because it can carry hostnames or query fragments.
	return "apply_failed"
}

// SweepOldRows is called periodically by the server (cmd/server) to remove
// settled rows older than 1h. Returns the count removed.
//
// Refuses to run with olderThan < 1 minute — that's almost certainly a
// misconfiguration and would delete in-flight outbox rows.
func (w *Worker) SweepOldRows(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan < time.Minute {
		return 0, fmt.Errorf("sweep: refusing olderThan=%v (< 1m would delete in-flight rows)", olderThan)
	}
	q := fmt.Sprintf(`
DELETE FROM %s.cache_invalidations
WHERE enqueued_at < now() - $1::interval`, w.cfg.Schema)
	tag, err := w.pool.Exec(ctx, q, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

var _ pgx.Conn // anchor for the pgx import; CI vet would flag unused otherwise
