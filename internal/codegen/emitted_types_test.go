package codegen

import (
	"context"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// The exact call sequence the emitter generates, compiled.
//
// # This file is no longer the primary check
//
// It was written when nothing compiled a generated server: the emitter tests
// call `parseAsGo`, which parses without type-checking, and the emitted code
// imported a pb path that resolved in no module. A review shipped a change
// where `runtime.CallerPartition` returned `(any, error)` and
// `queryresult.Hash` took a `string`, and every generated server for a
// partitioned entity failed to compile in the caller's repository while this
// suite stayed green — the emitter test asserted the broken text as correct.
//
// The real output is now compiled. internal/codegen/compilecheck holds the
// emitted server for the testdata fixture as an ordinary package, so
// `go build ./...` type-checks it, and TestEmittersMatchGolden keeps the
// committed copy identical to what the emitter produces. That is the substitute
// this comment used to say did not exist.
//
// # Why it is kept
//
// The fixture reaches most of the emitter's boundary but not all of it: a
// schema shape it does not contain is a call this package still does not
// compile. Snippets here cover those, and cost nothing.
//
// It was deliberately not deleted in the same change that introduced its
// replacement — removing the safety net and the proof at once leaves nothing to
// fall back on if the new one turns out to have a hole. Prefer extending the
// fixture over adding lines here; a case the fixture covers is covered by the
// compiler, which is stronger than a hand-written echo of it.

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
