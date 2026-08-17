package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The emitted server, committed as an ordinary package so the Go compiler
// type-checks it.
//
// # Why a committed package and not a test
//
// Nothing in this repository compiled a generated server. The emitter tests
// call parseAsGo, which parses without type-checking, and the emitted code
// imported a pb module that resolved nowhere — so a signature change on either
// side of the emitter boundary was discovered in a caller's build. That has
// happened: emitted_types_test.go records an incident where every generated
// server for a partitioned entity failed to compile in the caller's repository
// while this suite stayed green.
//
// A test that shells out to buf and go build would have to skip when buf is
// absent, and a skipping test is not a gate. Committing the output instead
// makes `go build ./...`, `go vet` and CI type-check it for free, with nothing
// to skip and no toolchain to install.
//
// # What it caught immediately
//
// On its first compile, four defects that every other test in this package had
// passed over:
//
//   - the emitted .proto named atlantis.common.v1.Interval without importing it
//   - the emitted server declared pgtype.Interval without importing pgtype
//   - runtime.IntervalToProto returned this module's commonpb.Interval, which
//     is a DIFFERENT Go type from the emitted server's copy of the same message
//   - the pb import path resolved in no module at all
//
// # Why this pinning function exists
//
// A committed package that compiles but no longer matches the emitter would be
// worse than no check: it would report the emitter healthy on the strength of a
// stale artifact. So the same run that compares the golden files compares this
// tree, and `-update` rewrites both.
const compilecheckServerDir = "compilecheck/server"

// compilecheckConfig points the emitter at the committed tree's own paths.
// Both prefixes are inside this module on purpose — the emitted server imports
// atlantis's internal/ packages, so it can only ever compile here.
var compilecheckConfig = GenConfig{
	ServerPBPrefix:  "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/pb",
	ServerPkgPrefix: "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/server",
}

// emitCompilecheckServer renders the fixture's server at the committed tree's
// import paths, keyed by path relative to compilecheckServerDir.
func emitCompilecheckServer(t *testing.T, ir *dsl.IR) map[string]string {
	t.Helper()
	files, err := EmitGoServer(ir, compilecheckConfig)
	if err != nil {
		t.Fatalf("EmitGoServer for compilecheck: %v", err)
	}
	const emitPrefix = "gen/go/server/"
	out := map[string]string{}
	for _, gf := range files {
		rel, ok := strings.CutPrefix(gf.Path, emitPrefix)
		if !ok {
			t.Fatalf("emitted server path %q does not start with %q; the "+
				"remapping below would put it somewhere unexpected", gf.Path, emitPrefix)
		}
		out[rel] = gf.Content
	}
	if len(out) == 0 {
		t.Fatal("the server emitter produced nothing for the fixture")
	}
	return out
}

func writeCompilecheckServer(t *testing.T, ir *dsl.IR) {
	t.Helper()
	if err := os.RemoveAll(compilecheckServerDir); err != nil {
		t.Fatalf("clear %s: %v", compilecheckServerDir, err)
	}
	for rel, content := range emitCompilecheckServer(t, ir) {
		dst := filepath.Join(compilecheckServerDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dst, err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", dst, err)
		}
	}
	t.Logf("rewrote %s — run `go build ./internal/codegen/compilecheck/...`, and "+
		"regenerate its pb with buf if the fixture's .proto changed", compilecheckServerDir)
}

func compareCompilecheckServer(t *testing.T, ir *dsl.IR) {
	t.Helper()
	want := emitCompilecheckServer(t, ir)
	for rel, content := range want {
		path := filepath.Join(compilecheckServerDir, rel)
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v\nThe committed compile fixture is what type-checks "+
				"the emitter. Regenerate with: go test ./internal/codegen -run Golden -update",
				path, err)
			continue
		}
		if string(onDisk) != content {
			t.Errorf("%s differs from what the emitter produces now.\n%s\n"+
				"A committed package that compiles but no longer matches the "+
				"emitter reports the emitter healthy on the strength of a stale "+
				"artifact. Regenerate with: "+
				"go test ./internal/codegen -run Golden -update",
				path, firstDifference(string(onDisk), content))
		}
	}

	// The reverse direction: a file on disk the emitter no longer produces is
	// dead code that still compiles, so `go build` would keep passing on an
	// artifact nothing generates.
	entries, err := os.ReadDir(compilecheckServerDir)
	if err != nil {
		t.Fatalf("read %s: %v", compilecheckServerDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("the compile fixture directory is empty, so `go build` compiles " +
			"nothing and the type check this package exists for is not happening")
	}
}
