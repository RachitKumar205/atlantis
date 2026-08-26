package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Batch wraps pgx.Batch with a small ergonomic shell.
//
// A bulk path doing per-row inserts issues one round trip per row: a 5k-row
// import costs 5k of them. Generated batch-insert handlers queue through this
// wrapper and so never name pgx.
//
// Usage:
//
//	b := pg.NewBatch()
//	for _, row := range rows {
//	    b.Queue("INSERT INTO ... VALUES ($1, $2)", row.A, row.B)
//	}
//	if err := pool.SendBatch(ctx, b); err != nil { ... }
//
// All queued statements run on a single connection inside one round trip.
type Batch struct {
	inner *pgx.Batch
}

// NewBatch returns an empty Batch ready to accept Queue calls.
func NewBatch() *Batch { return &Batch{inner: &pgx.Batch{}} }

// Queue appends a parameterized statement to the batch. Position in the
// batch matters; results come back in the same order.
func (b *Batch) Queue(sql string, args ...any) { b.inner.Queue(sql, args...) }

// Len returns how many statements are queued. Used by callers that want to
// decide between SendBatch and a single multi-row INSERT.
func (b *Batch) Len() int { return b.inner.Len() }

// SendBatch dispatches a batch on the pool, iterating results and returning the
// first error. A caller needing the per-statement Result — RETURNING values —
// calls SendBatchResults.
//
// Every queued Exec or Query must succeed; the first error short-circuits the
// rest.
func (p *Pool) SendBatch(ctx context.Context, b *Batch) error {
	if b == nil || b.Len() == 0 {
		return nil
	}
	res := p.pool.SendBatch(ctx, b.inner)
	defer func() { _ = res.Close() }()
	for range b.Len() {
		if _, err := res.Exec(); err != nil {
			return fmt.Errorf("batch statement: %w", err)
		}
	}
	return nil
}

// SendBatchResults returns the raw pgx.BatchResults, so RETURNING values can be
// pulled one statement at a time. The caller closes it.
func (p *Pool) SendBatchResults(ctx context.Context, b *Batch) pgx.BatchResults {
	return p.pool.SendBatch(ctx, b.inner)
}

// RunInTx executes fn inside a transaction, committing if fn returns nil and
// rolling back otherwise. ctx is threaded through, so one deadline covers the
// whole transaction.
//
// Generated code enqueues cache invalidation inside fn through
// runtime.Outbox.Enqueue, so the invalidation row commits with the data write.
//
// RunInTx does no post-commit work; anything that has to run after the commit
// runs outside fn.
func (p *Pool) RunInTx(ctx context.Context, fn func(ctx context.Context, tx runtime.Tx) error) error {
	tx, err := p.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Best-effort rollback. pgx returns ErrTxClosed on a transaction that
	// already committed, which is the ordinary path here.
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
