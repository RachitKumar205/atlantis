//go:build !embedspa

package main

import "io/fs"

// spaFS reports that no SPA is compiled in.
//
// # Why the embed is behind a build tag
//
// `//go:embed dist` does not compile when dist is absent, and dist is build
// output that nothing tracks. Without this split, `go build ./...` fails on a
// clean checkout, and every CI step that would cover this package has to be
// skipped — which is why `cmd/console` was in no vet, test or lint run for as
// long as it existed.
//
// A file excluded by a build constraint is never scanned for embed directives,
// so with the tag off there is no dist requirement anywhere in the build. That
// is the whole trick, and it is the shape Prometheus uses for the same reason
// (web/ui/ui.go versus web/ui/assets_embed.go).
//
// Untagged builds are the DEVELOPMENT build: the API runs, and the SPA comes
// from the Vite dev server on its own port. `make build-console` is untagged
// for that reason, and because it is a prerequisite of every `make dev-*`
// target — tagging it would make those fail to compile on a machine with no
// Node, which is a regression from what works today.
//
// The handler answers 404 naming the command that fixes it, so a binary built
// the wrong way says so on the first request rather than serving a blank page.
func spaFS() (fs.FS, error) { return nil, nil }
