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
// The emitter tests call parseAsGo, which parses without type-checking, so a
// signature change on either side of the emitter boundary reaches a caller's
// build before it reaches this suite. Committing the output makes
// `go build ./...`, `go vet` and CI type-check it, with no toolchain to install
// and nothing to skip when buf is absent.
//
// The defects a type-check catches and a parse does not:
//
//   - a .proto naming atlantis.common.v1.Interval without importing it
//   - a server declaring pgtype.Interval without importing pgtype
//   - runtime.IntervalToProto returning this module's commonpb.Interval, a
//     different Go type from the emitted server's copy of the same message
//   - a pb import path that resolves in no module
//
// A committed package that compiles but no longer matches the emitter would
// report the emitter healthy from a stale artifact, so the same run that
// compares the golden files compares this tree, and `-update` rewrites both.
const compilecheckServerDir = "compilecheck/server"

// compilecheckConfig points the emitter at the committed tree's own paths.
// Both prefixes are inside this module: the emitted server imports atlantis's
// internal/ packages, so it can only compile here.
var compilecheckConfig = GenConfig{
	ServerPBPrefix:  "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/pb",
	ServerPkgPrefix: "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/server",
}

// emitCompilecheckServer renders the fixture's server at the committed tree's
// import paths, keyed by path relative to compilecheckServerDir.
func emitCompilecheckServer(t *testing.T, ir *dsl.IR) map[string]string {
	t.Helper()

	// Both server-side Go emitters. EmitCustomServer is here because it emits
	// the same KIND of code as EmitGoServer — importing pb and runtime, with
	// the same pb prefix — and its output had never been compiled either. It
	// carried the identical unresolvable `atlantis-go` import.
	//
	// Every server-side emitter belongs in this list. The client, keys, jobs,
	// workflow and ephemeral emitters are not here yet; see the coverage note
	// on TestCompilecheckCoversTheServerEmitters.
	var files []GoFile
	entityFiles, err := EmitGoServer(ir, compilecheckConfig)
	if err != nil {
		t.Fatalf("EmitGoServer for compilecheck: %v", err)
	}
	files = append(files, entityFiles...)

	customFiles, err := EmitCustomServer(ir, compilecheckConfig)
	if err != nil {
		t.Fatalf("EmitCustomServer for compilecheck: %v", err)
	}
	files = append(files, customFiles...)

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
		t.Fatal("the server emitters produced nothing for the fixture")
	}
	return out
}

// Everything the emitters put under gen/go/server/ must be compiled.
//
// emitCompilecheckServer names its emitters by hand, so one added to
// cmd/tidectl and not to that list emits server code this package never builds.
//
// The check is derived rather than listed: the golden set already holds every
// emitter's output for the fixture, so anything in it under gen/go/server/ and
// absent from the compile fixture is a hole, whatever emitter produced it.
//
// Server side only. gen/go/keys and clients/go/client are emitted and
// golden-compared but not compiled: the client is caller code that imports the
// caller's own pb, so compiling it here would need a second fixture module.
func TestCompilecheckCoversTheServerEmitters(t *testing.T) {
	const serverPrefix = "gen/go/server/"
	goldenDir := filepath.Join("testdata", "golden")

	want, err := readGolden(goldenDir)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var serverFiles []string
	for path := range want {
		if strings.HasPrefix(path, serverPrefix) && strings.HasSuffix(path, ".go") {
			serverFiles = append(serverFiles, strings.TrimPrefix(path, serverPrefix))
		}
	}
	if len(serverFiles) == 0 {
		t.Fatal("the golden set has no files under gen/go/server/, so this test " +
			"is comparing two empty sets and proving nothing")
	}

	for _, rel := range serverFiles {
		if _, err := os.Stat(filepath.Join(compilecheckServerDir, rel)); err != nil {
			t.Errorf("%s%s is emitted but has no counterpart in %s, so it is never "+
				"compiled. Add its emitter to emitCompilecheckServer: %v",
				serverPrefix, rel, compilecheckServerDir, err)
		}
	}
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
