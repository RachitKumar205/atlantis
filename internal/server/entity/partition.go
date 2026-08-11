package entity

import (
	"context"
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// querier is the part of runtime.Pool and runtime.Tx a statement needs. It
// exists so one call site can run either directly on the pool or inside a
// transaction without knowing which.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) runtime.Row
	Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (runtime.CommandTag, error)
}

// scopedRead runs fn against the database, inside a tenant-bound transaction
// when the entity declares `partition by`.
//
// # Why reads need a transaction at all
//
// The tenant is a transaction-local run-time parameter, which is what makes it
// safe on a pooled connection: it reverts when the transaction ends, so a later
// request reusing the same backend cannot inherit it. A statement on the bare
// pool has no transaction to bind, so it would run with no tenant set — seeing
// nothing on a role row-level security applies to, and everything on a role
// that bypasses it.
//
// # Why only partitioned entities pay for it
//
// An entity without `partition by` runs exactly as before, straight on the
// pool. That is the overwhelming majority of entities, and a transaction per
// read is not a cost worth imposing on them to serve a clause they do not use.
// The branch is on a field resolved once at snapshot time, not per request.
//
// Binding itself is cheap: migration 0024 made the discriminator a run-time
// parameter, so a bind writes nothing and assigns no transaction ID. Under the
// table-backed design this function would have cost one transaction ID per
// read, which is why it was not written then.
//
// # Fails closed
//
// For a partitioned entity with no tenant in context this returns an error and
// reads nothing. It must not fall through to an unbound read: that is correct
// only on a deployment whose database role obeys row-level security, and the
// whole point of enforcing in the database is not to depend on the caller
// having got something else right.
//
// # Errors are the caller's, not this function's
//
// fn's error is returned as-is. An earlier draft swallowed it and returned
// whatever Commit said, which on an aborted transaction is
// "commit unexpectedly resulted in rollback" — so permission denied, statement
// timeout and undefined column all arrived as the same sentence.
func (s *Server) scopedRead(ctx context.Context, meta *entityMeta, fn func(q querier) error) error {
	return s.scopedReadIf(ctx, meta.partitioned, fn)
}

// scopedReadIf is scopedRead driven by an already-resolved flag, for the read
// paths that are not scoped to a single entity: a custom query names its own
// touched set, and any one of them declaring `partition by` is enough.
func (s *Server) scopedReadIf(ctx context.Context, partitioned bool, fn func(q querier) error) error {
	if !partitioned {
		return fn(s.pool)
	}

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("partitioned read: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// context.Background(), not ctx. pgx calls conn.die() when the rollback
		// Exec fails, so rolling back on a cancelled context destroys the
		// pooled connection instead of returning it — turning every client
		// disconnect into a connection churn.
		_ = tx.Rollback(context.Background())
	}()

	if err := runtime.BindPartition(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	// Committing a read-only transaction releases its snapshot. Rollback would
	// do as well; commit is what says "this completed".
	//
	// context.Background(), for the same reason as the rollback above and then
	// some. Committing on the request context fails every read whose client
	// disconnected — after the rows were already materialised — and destroys
	// the pooled connection on the way out. Measured over 30 reads cancelled
	// immediately after the last row: 30 of 30 failed and 29 new connections
	// were opened, against 0 and 0 for the same shape committing on
	// Background. The rows are already in hand by this point; there is nothing
	// left for cancellation to save.
	if err := tx.Commit(context.Background()); err != nil {
		return fmt.Errorf("partitioned read: commit: %w", err)
	}
	committed = true
	return nil
}

// bindWrite binds the tenant on a transaction a write path has already opened.
//
// Writes are transactional already, so they need the binding rather than the
// transaction. Kept separate from scopedRead so the write path keeps control of
// its own transaction — it has commit and rollback semantics that matter, and
// wrapping them would obscure that.
//
// Call it before any other statement in the transaction. The policy applies
// from the moment a statement runs, so a read or write issued ahead of the bind
// is unscoped.
func (s *Server) bindWrite(ctx context.Context, meta *entityMeta, tx runtime.Tx) error {
	if !meta.partitioned {
		return nil
	}
	return runtime.BindPartition(ctx, tx)
}
