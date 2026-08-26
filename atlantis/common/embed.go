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
// A glob rather than a file list, which has to be edited alongside every new
// common proto. Omitted from the list, a proto reaches no caller and their
// `buf generate` fails on an import it cannot resolve.
//
//go:embed v1/*.proto
var Protos embed.FS
