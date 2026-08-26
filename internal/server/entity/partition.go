package entity

import (
	"context"

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
// A read needs a transaction because the tenant is a transaction-local run-time
// parameter, which is what makes it safe on a pooled connection: it reverts
// when the transaction ends, so a later request reusing the backend cannot
// inherit it. A statement on the bare pool has no transaction to bind and runs
// with no tenant set, seeing nothing on a role RLS applies to and everything on
// a role that bypasses it.
//
// An entity without `partition by` runs straight on the pool. The branch is on
// a field resolved once at snapshot time, not per request. Binding is cheap:
// since migration 0024 the discriminator is a run-time parameter, so a bind
// writes nothing and assigns no transaction id.
//
// Fails closed. A partitioned entity with no tenant in context returns an error
// and reads nothing rather than falling through to an unbound read, which is
// correct only on a deployment whose database role obeys row-level security.
//
// fn's error is returned as-is. Returning Commit's error instead reports
// "commit unexpectedly resulted in rollback" for permission denied, statement
// timeout and undefined column alike.
func (s *Server) scopedRead(ctx context.Context, meta *entityMeta, fn func(q querier) error) error {
	return s.scopedReadIf(ctx, meta.partitioned, fn)
}

// scopedReadIf is scopedRead driven by an already-resolved flag, for the read
// paths that are not scoped to a single entity: a custom query names its own
// touched set, and any one of them declaring `partition by` is enough.
func (s *Server) scopedReadIf(ctx context.Context, partitioned bool, fn func(q querier) error) error {
	// Delegates to runtime.ScopedRead, which the code-generated caller server
	// also calls. This used to be the implementation, and the emitter had no
	// equivalent at all — the feature was correct in the surface that is easy
	// to test and absent from the surface callers deploy. One function, two
	// callers, no drift.
	return runtime.ScopedRead(ctx, s.pool, partitioned, func(q runtime.Querier) error {
		return fn(q)
	})
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
	return runtime.BindWrite(ctx, meta.partitioned, tx)
}
