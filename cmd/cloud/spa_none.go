//go:build !embedspa

package main

import "io/fs"

// spaFS reports that no sign-in application is compiled in.
//
// # Why the embed is behind a build tag
//
// `//go:embed dist` does not compile when dist is absent, and dist is build
// output that nothing tracks. Without this split, `go build ./...` fails on a
// clean checkout, and no CI step can cover this package at all — which is the
// state cmd/cloud and cmd/console were both in.
//
// A file excluded by a build constraint is never scanned for embed directives,
// so with the tag off there is no dist requirement anywhere in the build.
// Prometheus splits web/ui/ui.go from web/ui/assets_embed.go for the same
// reason.
//
// # Untagged is the development build
//
// `make dev-auth` runs the API; the sign-in application comes from the Vite dev
// server on its own port, proxying /api here. So nothing in the dev loop wants
// an embedded SPA — and `build-cloud` must stay untagged because dev-auth,
// dev-cloud-seed, dev-org-register and dev-token all depend on it. Tagging it
// would make all four fail to compile on a machine with no Node, which works
// today.
//
// The handler answers 404 naming the command that fixes it, so a binary built
// the wrong way says so on the first request rather than serving a blank page.
func spaFS() (fs.FS, error) { return nil, nil }
