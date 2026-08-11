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
// # Why this lives in runtime and not beside one of its callers
//
// atlantis serves entities from two places: the dynamic dispatcher in
// internal/server/entity, and the Go server that `tidectl codegen` emits into a
// caller's repository. They are separate code paths that answer the same
// requests, and the second is the one callers actually deploy.
//
// The dispatcher was fixed to bind on all eight of its paths. The emitter was
// not touched, and a review found the generated server binding on NONE of Get,
// List, BatchGet, Create, Update or Delete — the whole feature was correct in
// the surface that is easy to test and absent from the surface that ships.
//
// A second copy of this logic in the emitter would have the same future. One
// function, called by both, cannot drift.
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
//
// Binding itself is cheap: migration 0024 made the discriminator a run-time
// parameter, so a bind writes nothing and assigns no transaction ID.
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
