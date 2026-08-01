package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReapParkedJobName is the canonical id for the built-in reaper.
const ReapParkedJobName = "atlantis.ReapParked"

// ReapParkedHandler drops objects parked by a destructive migration once their
// retention window has passed.
//
// Destructive changes park rather than drop: a column is renamed out of the
// way, a table is moved to atlantis_tombstone, and the rows stay. That makes
// the change reversible; it also means the objects accumulate until something
// removes them. This is that something.
//
// The reaper is the half of park-and-reap that costs money if it is missing and
// costs data if it is wrong, so it is deliberately conservative:
//
//   - It only ever touches rows in atlantis.parked_objects. It does not go
//     looking for things that match a naming convention, because a table an
//     operator renamed by hand to something ending in __parked is not a
//     tombstone and must not be dropped on that evidence.
//   - It reaps one object per statement and records each one before moving on,
//     so a failure part-way leaves an accurate register rather than a set of
//     drops nobody can account for.
//   - A drop that fails is logged and skipped, not retried in a loop. The
//     usual cause is a dependency added after the park, and repeatedly failing
//     to drop is much better than cascading.
type ReapParkedHandler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// BatchLimit bounds one invocation. Reaping is not urgent — every object
	// here is already invisible to the application — so a small batch on a
	// frequent schedule is preferable to a long-running statement holding
	// locks.
	BatchLimit int

	// LockTimeout bounds how long a reap will wait for its lock.
	//
	// Dropping a parked column is ALTER TABLE on a LIVE table, which needs
	// ACCESS EXCLUSIVE. Without a timeout the reaper waits behind any open
	// transaction touching that table — and while it waits, every subsequent
	// query queues behind IT, because a pending ACCESS EXCLUSIVE blocks the
	// readers that would otherwise have proceeded. A background job with no
	// deadline would take the table down to delete something nobody can see.
	//
	// Giving up is free: the registration stays, and the next run tries again.
	LockTimeout time.Duration
}

// DefaultReapLockTimeout is short on purpose. Reaping has no deadline, so
// waiting buys nothing and risks everything.
const DefaultReapLockTimeout = 3 * time.Second

type parkedRow struct {
	id           int64
	kind         string
	schemaName   string
	objectName   string
	parentTable  string
	originalName string
}

// Handle drops every parked object whose window has passed.
func (h *ReapParkedHandler) Handle(ctx context.Context, _ []byte) error {
	limit := h.BatchLimit
	if limit <= 0 {
		limit = 50
	}

	// next_attempt_after keeps a drop that cannot succeed from being retried on
	// every run. Without that clause the same failing rows are selected forever
	// and everything behind them is never reaped — see the column's comment in
	// migration 0022.
	rows, err := h.Pool.Query(ctx, `
SELECT id, kind, schema_name, object_name, coalesce(parent_table, ''), original_name
  FROM atlantis.parked_objects
 WHERE reaped_at IS NULL
   AND reap_after <= now()
   AND (next_attempt_after IS NULL OR next_attempt_after <= now())
 ORDER BY reap_after
 LIMIT $1`, limit)
	if err != nil {
		return fmt.Errorf("reap: list due objects: %w", err)
	}

	// Collected before dropping anything. Holding the cursor open across DDL
	// on the same pool is how a reaper deadlocks against itself.
	var due []parkedRow
	for rows.Next() {
		var p parkedRow
		if err := rows.Scan(&p.id, &p.kind, &p.schemaName, &p.objectName,
			&p.parentTable, &p.originalName); err != nil {
			rows.Close()
			return fmt.Errorf("reap: scan: %w", err)
		}
		due = append(due, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reap: iterate: %w", err)
	}

	reaped, failed := 0, 0
	for _, p := range due {
		if err := h.reapOne(ctx, p); err != nil {
			// Two expected causes, neither worth escalating: a lock this reap
			// declined to wait for, and a dependency created after the park.
			// Both leave the registration intact, backed off, for a later run —
			// which is why this is not a retry loop, and why the alternative to
			// a failed drop is never CASCADE, which would take the dependency
			// with it.
			h.log().Warn("reap: drop failed, leaving parked",
				"id", p.id, "kind", p.kind, "object", p.describe(), "err", err)
			h.recordFailure(ctx, p, err)
			failed++
			continue
		}
		h.log().Info("reap: dropped parked object",
			"kind", p.kind, "object", p.describe(), "was", p.originalName)
		reaped++
	}
	if len(due) > 0 {
		h.log().Info("reap: batch complete",
			"due", len(due), "reaped", reaped, "failed", failed)
	}
	return nil
}

// reapOne drops a single parked object and records the drop, in one
// transaction.
//
// The two are one transaction because the alternative — drop, then stamp on a
// second round trip — has a window where the object is gone and the register
// still calls it parked. That register is the only record the object ever
// existed and the only list an operator can consult, so it must not be able to
// disagree with the database about what is there. Postgres makes DDL
// transactional, so this costs nothing.
//
// An explicit pgx transaction rather than a multi-statement string: when a
// statement inside a simple-protocol "BEGIN; ...; COMMIT;" fails, Postgres
// skips the rest INCLUDING the COMMIT, and the connection goes back to the
// pool inside a failed transaction. pgxpool discards such a connection, so
// every failed reap silently destroyed and rebuilt a backend on the pool that
// also serves auth and the admin plane. pgx issues a real ROLLBACK here and
// the connection is reusable.
func (h *ReapParkedHandler) reapOne(ctx context.Context, p parkedRow) error {
	drop, err := reapStatement(p)
	if err != nil {
		return err
	}

	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SET LOCAL, so the bound dies with the transaction rather than persisting
	// on a pooled connection and applying to whoever checks it out next.
	ms := h.LockTimeout.Milliseconds()
	if ms <= 0 {
		ms = DefaultReapLockTimeout.Milliseconds()
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", ms)); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, drop); err != nil {
		return err
	}
	// AND reaped_at IS NULL so that two servers reaping concurrently cannot
	// both record the drop; the loser commits nothing and reports no reap.
	if _, err := tx.Exec(ctx,
		`UPDATE atlantis.parked_objects SET reaped_at = now() WHERE id = $1 AND reaped_at IS NULL`,
		p.id); err != nil {
		return fmt.Errorf("record reap: %w", err)
	}
	return tx.Commit(ctx)
}

// recordFailure counts the attempt and pushes the next one out.
//
// Best-effort: this runs after a reap already failed, and a failure to record
// the failure must not take the rest of the batch down with it. The cost of
// losing this write is one wasted retry.
func (h *ReapParkedHandler) recordFailure(ctx context.Context, p parkedRow, cause error) {
	const backoff = `
UPDATE atlantis.parked_objects
   SET attempts = attempts + 1,
       last_error = $2,
       next_attempt_after = now()
         + least(interval '1 hour' * power(2, least(attempts, 6)), interval '24 hours')
 WHERE id = $1`
	if _, err := h.Pool.Exec(ctx, backoff, p.id, cause.Error()); err != nil {
		h.log().Warn("reap: could not record the failed attempt; it will be "+
			"retried on the next run instead of backing off",
			"id", p.id, "err", err)
	}
}

// describe names a parked object the way an operator needs to see it: a column
// is meaningless without the table holding it, and this log line is the only
// record of the drop outside the register.
func (p parkedRow) describe() string {
	if p.parentTable != "" {
		return p.schemaName + "." + p.parentTable + "." + p.objectName
	}
	return p.schemaName + "." + p.objectName
}

func (h *ReapParkedHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// reapStatement renders the DDL that removes one parked object.
//
// Identifiers come from the register, which the generated migration wrote from
// schema names — but they are still interpolated into DDL, so they are quoted
// here rather than trusted. An identifier that cannot be quoted safely is
// refused: dropping the wrong object is worse than dropping nothing.
func reapStatement(p parkedRow) (string, error) {
	switch p.kind {
	case "table":
		if p.schemaName == "" || p.objectName == "" {
			return "", fmt.Errorf("table registration is missing a schema or name")
		}
		// No CASCADE, here or below. If something depends on a tombstone the
		// drop should fail and be looked at, not take the dependency with it —
		// and for a column the dependency would be views, FKs and indexes on a
		// LIVE table.
		return fmt.Sprintf("DROP TABLE IF EXISTS %s.%s",
			quoteIdent(p.schemaName), quoteIdent(p.objectName)), nil
	case "column":
		if p.schemaName == "" || p.parentTable == "" || p.objectName == "" {
			return "", fmt.Errorf("column registration is missing a schema, table or name")
		}
		return fmt.Sprintf("ALTER TABLE %s.%s DROP COLUMN IF EXISTS %s",
			quoteIdent(p.schemaName), quoteIdent(p.parentTable), quoteIdent(p.objectName)), nil
	default:
		return "", fmt.Errorf("unknown parked kind %q", p.kind)
	}
}

// quoteIdent double-quotes a SQL identifier, doubling any embedded quote.
func quoteIdent(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '"' {
			out = append(out, '"')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}
