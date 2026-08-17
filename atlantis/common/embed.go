// Package common embeds the static common proto definitions (predicates,
// pagination) so tools can materialize them without a checkout of the
// atlantis repo. The embed paths point at the canonical files in this
// directory's v1/ subtree — there is no copy to drift out of sync.
package common

import "embed"

// Protos holds atlantis/common/v1/*.proto. tide generate writes these into
// a caller's output dir as compile-time inputs so a caller's namespace
// protos can `import "atlantis/common/v1/...";`.
//
// A glob rather than a file list. The list was hand-maintained, so a new
// common proto reached callers only if whoever added it also remembered this
// line — and the failure is quiet in the worst place: the caller's `buf
// generate` fails on an import it cannot resolve, in their repository.
//
//go:embed v1/*.proto
var Protos embed.FS
