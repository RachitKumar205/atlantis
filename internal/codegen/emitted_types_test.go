package codegen

import (
	"context"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The exact call sequence the emitter generates, compiled.
//
// Not the primary check. internal/codegen/compilecheck holds the emitted server
// for the testdata fixture as an ordinary package, so `go build ./...`
// type-checks it, and TestEmittersMatchGolden keeps the committed copy
// identical to what the emitter produces.
//
// The fixture reaches most of the emitter's boundary but not all of it: a
// schema shape it does not contain is a call nothing compiles. The snippets
// here cover those. Prefer extending the fixture over adding lines here, since
// a case the fixture covers is covered by the compiler.

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
