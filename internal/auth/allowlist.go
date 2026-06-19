// Package auth implements caller-identity enforcement for the atlantis
// gRPC surface. The allowlist is the union of two sources, loaded on
// startup and refreshed on a periodic ticker:
//
//   - atlantis.caller_registrations — callers that have applied schema
//     (a row appears on their first admin.ApplyMigration).
//   - atlantis.caller_identities — callers an operator pre-registered
//     (console / RegisterCaller), including read-only runtime CNs that
//     only ever open a typed client connection and never apply schema.
//
// A caller becomes callable within one refresh interval of landing in
// either table. This gate only decides whether a caller may open the
// gRPC surface at all; mutation is gated separately
// (caller_identities.can_mutate ∪ ATL_MUTATION_ALLOWED_CALLERS).
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CallerAllowlist is a snapshot of the distinct callers known to the
// server — the union of atlantis.caller_registrations and
// atlantis.caller_identities. The auth interceptor checks every
// non-exempt RPC against this set. Reload swaps the set atomically;
// readers never see a partial state.
type CallerAllowlist struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	mu  sync.RWMutex
	set map[string]struct{}
}

// New returns an empty allowlist bound to pool. Call Reload before
// gating any RPCs against it — a fresh-process Allows() returns false
// for everything until the first successful Reload.
func New(pool *pgxpool.Pool, log *slog.Logger) *CallerAllowlist {
	if log == nil {
		log = slog.Default()
	}
	return &CallerAllowlist{
		pool: pool,
		log:  log,
		set:  map[string]struct{}{},
	}
}

// Reload reads the full set of callers — caller_registrations (applied
// schema) UNION caller_identities (operator pre-registered, including
// read-only runtime CNs) — and swaps it in atomically. Errors propagate
// so the caller can decide whether to abort startup or log and continue
// with the previous snapshot.
func (a *CallerAllowlist) Reload(ctx context.Context) error {
	rows, err := a.pool.Query(ctx, `
		SELECT caller FROM atlantis.caller_registrations
		UNION
		SELECT caller FROM atlantis.caller_identities`)
	if err != nil {
		return fmt.Errorf("auth: query caller allowlist: %w", err)
	}
	defer rows.Close()
	fresh := map[string]struct{}{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return fmt.Errorf("auth: scan caller: %w", err)
		}
		fresh[c] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	a.set = fresh
	a.mu.Unlock()
	return nil
}

// Allows reports whether caller appears in the most recent snapshot.
func (a *CallerAllowlist) Allows(caller string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.set[caller]
	return ok
}

// Size returns the current snapshot size — useful for /readyz, logs,
// and tests.
func (a *CallerAllowlist) Size() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.set)
}

// RunRefresher reloads the allowlist on a fixed interval until ctx
// cancels. Errors log at WARN; the previous snapshot keeps serving so
// a transient PG blip can't lock every caller out of the gRPC surface.
// interval <= 0 falls back to 30s.
func (a *CallerAllowlist) RunRefresher(ctx context.Context, interval time.Duration) {
	defer func() {
		if rec := recover(); rec != nil {
			a.log.Error("allowlist refresher panic", "panic", rec)
		}
	}()
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.Reload(ctx); err != nil {
				a.log.Warn("allowlist reload", "err", err)
			}
		}
	}
}
