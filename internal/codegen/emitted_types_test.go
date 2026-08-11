package codegen

import (
	"context"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The exact call sequence the emitter generates, compiled.
//
// # Why this file exists
//
// Nothing compiles a generated server. The emitter tests call `parseAsGo`,
// which parses and does not typecheck, and the generated code imports a
// caller's `atlantis-go` protobuf package that is not a dependency of this
// module — so CI never builds any of it.
//
// A review shipped an emitter change where `runtime.CallerPartition` returns
// `(any, error)` and `queryresult.Hash` takes a `string`. Every generated
// server for a partitioned entity failed to compile in the caller's repository.
// The whole suite here was green, and the emitter test even asserted the
// broken text as if it were correct.
//
// So the type boundaries the emitter depends on are written out here as real
// Go. They are never called. If a signature on either side moves, this file
// stops compiling and the failure lands in THIS repository rather than in a
// caller's build.
//
// Add a line here whenever the emitter starts calling something new across a
// package boundary. It is not a substitute for compiling the real output — see
// the note in partition_emit_test.go — but it catches the shape that broke.

var _ = func(ctx context.Context) (string, error) {
	// Emitted at the top of Query<E> for a `partition by` entity.
	partitionVal, err := runtime.PartitionKey(ctx)
	if err != nil {
		return "", err
	}
	// Emitted as the query-result cache key. partitionVal must satisfy Hash's
	// second parameter without a conversion, because the emitter writes the
	// identifier straight in.
	return queryresult.Hash("ns.Entity", partitionVal, nil, nil, 0, nil, nil, nil, 0)
}

var _ = func(ctx context.Context, pool runtime.Pool) error {
	// Emitted around every read on a partitioned entity.
	return runtime.ScopedRead(ctx, pool, true, func(q runtime.Querier) error {
		_, err := q.Query(ctx, "SELECT 1")
		return err
	})
}

var _ = func(ctx context.Context, tx runtime.Tx) error {
	// Emitted as the first statement of every write transaction.
	return runtime.BindWrite(ctx, true, tx)
}

// And the predicate argument: partitionVal is spliced into the args slice for
// `<partition field> = $1`, which takes `any`. A string satisfies that, so this
// only fails if PartitionKey stops returning something assignable to any.
var _ = func(ctx context.Context) []any {
	partitionVal, _ := runtime.PartitionKey(ctx)
	args := []any{}
	return append([]any{partitionVal}, args...)
}
