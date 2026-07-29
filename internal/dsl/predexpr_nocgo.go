//go:build !cgo

package dsl

import "fmt"

// lowerPredicate refuses in a cgo-free build. Validating a partial-index `where`
// predicate means parsing it with Postgres's own grammar via pg_query_go, which
// requires cgo; there is no pure-Go substitute, and approximating the grammar
// here would accept predicates Postgres later rejects at CREATE INDEX time.
//
// Refusing is safe for the binary this build exists to produce. `tide` ships raw
// .atl bytes to the server for plan and apply — the server does all parsing and
// lowering — so no cgo-free tide subcommand reaches this path. Only `tide
// sandbox`, which lowers locally, does, and it is excluded from cgo-free builds
// by its own build tag.
func lowerPredicate(raw string) (*PredExpr, error) {
	return nil, fmt.Errorf(
		"partial index: validating a `where` predicate requires a cgo-enabled build "+
			"(predicate parsing delegates to Postgres via pg_query_go); this binary was "+
			"built with CGO_ENABLED=0. Predicate was: %q", raw)
}
