package runtime

import (
	"context"
	"fmt"
)

// Querier is the part of Pool and Tx a single statement needs. It exists so one
// call site can run either directly on the pool or inside a transaction without
// knowing which.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
}

// ScopedRead runs fn against the database, inside a tenant-bound transaction
// when the entity declares `partition by`.
//
// It lives in runtime because two code paths serve the same requests: the
// dynamic dispatcher in internal/server/entity, and the Go server `tidectl
// codegen` emits into a caller's repository. One function called by both cannot
// drift. A second copy in the emitter binds on whichever verbs that copy
// remembers, and one binding on none of Get, List, BatchGet, Create, Update or
// Delete reads as working.
//
// A read needs a transaction because the tenant is a transaction-local run-time
// parameter, which is what makes it safe on a pooled connection: it reverts
// when the transaction ends, so a later request reusing the backend cannot
// inherit it. A statement on the bare pool has no transaction to bind and runs
// with no tenant set, seeing nothing on a role RLS applies to and everything on
// a role that bypasses it.
//
// An entity without `partition by` runs straight on the pool, so it pays no
// transaction. Binding is cheap: since migration 0024 the discriminator is a
// run-time parameter, so a bind writes nothing and assigns no transaction id.
//
// Fails closed. A partitioned entity with no tenant in context returns an error
// and reads nothing rather than falling through to an unbound read, which is
// correct only on a deployment whose database role obeys row-level security.
//
// fn's error is returned as-is. Returning Commit's error instead reports
// "commit unexpectedly resulted in rollback" for permission denied, statement
// timeout and undefined column alike.
func ScopedRead(ctx context.Context, pool Pool, partitioned bool, fn func(q Querier) error) error {
	if !partitioned {
		return fn(pool)
	}

	tx, err := pool.BeginTx(ctx)
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
		// disconnect into connection churn.
		_ = tx.Rollback(context.Background())
	}()

	if err := BindPartition(ctx, tx); err != nil {
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

// BindWrite binds the tenant on a transaction a write path has already opened.
//
// Writes are transactional already, so they need the binding rather than the
// transaction. Kept separate from ScopedRead so the write path keeps control of
// its own transaction — it has commit and rollback semantics that matter, and
// wrapping them would obscure that.
//
// Call it before any other statement in the transaction. The policy applies
// from the moment a statement runs, so a read or write issued ahead of the bind
// is unscoped.
func BindWrite(ctx context.Context, partitioned bool, tx Tx) error {
	if !partitioned {
		return nil
	}
	return BindPartition(ctx, tx)
}

// ScopedQuerier returns a Querier bound to the caller's tenant, and a release
// function the caller must defer.
//
// The closure form above is the better shape and should be preferred. This one
// exists for the code-generated custom-query handler, whose scan loop differs
// per declared output shape — wrapping each variant in a closure would mean
// restructuring all of them, and the version of that change most likely to be
// written is the one that wraps some variants and not others.
//
// The transaction is released by ROLLBACK, not COMMIT. These are reads, and a
// read-only transaction has nothing to commit; rollback releases the snapshot
// just as well. That also means release() is correct on every path, including
// the ones that return early on a scan error.
//
// For an unpartitioned entity this returns the pool and a release that does
// nothing, so nothing changes for the entities that declare no partition.
func ScopedQuerier(ctx context.Context, pool Pool, partitioned bool) (Querier, func(), error) {
	if !partitioned {
		return pool, func() {}, nil
	}
	tx, err := pool.BeginTx(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("partitioned read: begin: %w", err)
	}
	if err := BindPartition(ctx, tx); err != nil {
		// context.Background() for the same reason as elsewhere in this file:
		// rolling back on a cancelled context destroys the pooled connection.
		_ = tx.Rollback(context.Background())
		return nil, nil, err
	}
	return tx, func() { _ = tx.Rollback(context.Background()) }, nil
}
