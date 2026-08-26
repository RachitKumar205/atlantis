//go:build !embedspa

package main

import "io/fs"

// spaFS reports that no SPA is compiled in.
//
// The embed is behind a build tag because `//go:embed dist` does not compile
// when dist is absent, and dist is untracked build output. A file excluded by a
// build constraint is never scanned for embed directives, so with the tag off
// nothing in the build requires dist. Prometheus splits web/ui/ui.go from
// web/ui/assets_embed.go for the same reason.
//
// Untagged is the development build: the API runs and the SPA comes from the
// Vite dev server on its own port. `make build-console` stays untagged because
// every `make dev-*` target depends on it compiling without Node installed.
//
// The handler answers 404 naming the command that fixes it, so a binary built
// the wrong way says so on the first request rather than serving a blank page.
func spaFS() (fs.FS, error) { return nil, nil }
