package runtime

import (
	"context"
	"errors"
)

// ctxKey is a private type so callers can't manufacture context keys that
// collide with these. Standard "unexported struct" idiom.
type ctxKey int

const (
	ctxKeyCallerPartition ctxKey = iota
)

// ErrNoCallerPartition is returned when generated QueryX handlers ask for
// the caller's partition value on a partition-by-protected entity and the
// auth interceptor hasn't put one into ctx. Fail-closed: a partition-
// guarded entity must never serve queries with no partition value, or
// every row is exposed cross-tenant.
var ErrNoCallerPartition = errors.New("atlantis: no caller partition in context")

// CallerPartition returns the value the auth layer attached to ctx as the
// caller's partition identifier (e.g. their consumer_id). Generated
// QueryX handlers on entities declared `partition by <field>` invoke
// this; if it returns ErrNoCallerPartition, the handler aborts with
// Unauthenticated rather than fall through to a query that would expose
// all partitions.
//
// Today this is a stub: the auth interceptor that populates the context
// lands in a later phase. Until then any entity declared partition-by
// will return Unauthenticated for every request — which is the correct
// failure mode (closed) for the missing-auth case.
func CallerPartition(ctx context.Context) (any, error) {
	if v := ctx.Value(ctxKeyCallerPartition); v != nil {
		return v, nil
	}
	return nil, ErrNoCallerPartition
}

// WithCallerPartition attaches a caller partition value to ctx. Used by
// auth interceptors and by tests that need to exercise partition-guarded
// handlers end-to-end.
func WithCallerPartition(ctx context.Context, v any) context.Context {
	return context.WithValue(ctx, ctxKeyCallerPartition, v)
}
