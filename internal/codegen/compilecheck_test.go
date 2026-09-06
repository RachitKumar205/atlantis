package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// The emitted Go, committed as ordinary packages so the Go compiler
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
// compares the golden files compares these trees, and `-update` rewrites both.

// compilecheckConfig points the emitters at the committed trees' own paths,
// all inside this module. The emitted server imports atlantis's internal/
// packages and can compile nowhere else; the client compiles wherever its pb
// resolves, and compilecheck/pb is where the fixture's pb is committed.
var compilecheckConfig = GenConfig{
	ModulePrefix:    "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck",
	ServerPBPrefix:  "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/pb",
	ServerPkgPrefix: "github.com/rachitkumar205/atlantis/internal/codegen/compilecheck/server",
}

// emittedFile is one emitted file, whatever language produced it.
//
// GoFile and PyFile are separate types so Python bytes cannot reach
// format.Source, which means this machinery cannot take either directly.
// Adapters below convert; nothing else in the package uses this shape.
type emittedFile struct {
	Path    string
	Content string
}

type compilecheckEmitter struct {
	name string
	fn   func(*dsl.IR, GenConfig) ([]emittedFile, error)
}

// goEmitter adapts an emitter that produces Go.
func goEmitter(fn func(*dsl.IR, GenConfig) ([]GoFile, error)) func(*dsl.IR, GenConfig) ([]emittedFile, error) {
	return func(ir *dsl.IR, cfg GenConfig) ([]emittedFile, error) {
		got, err := fn(ir, cfg)
		if err != nil {
			return nil, err
		}
		out := make([]emittedFile, 0, len(got))
		for _, f := range got {
			out = append(out, emittedFile{Path: f.Path, Content: f.Content})
		}
		return out, nil
	}
}

// pyEmitter adapts an emitter that produces Python.
func pyEmitter(fn func(*dsl.IR) ([]PyFile, error)) func(*dsl.IR, GenConfig) ([]emittedFile, error) {
	return func(ir *dsl.IR, _ GenConfig) ([]emittedFile, error) {
		got, err := fn(ir)
		if err != nil {
			return nil, err
		}
		out := make([]emittedFile, 0, len(got))
		for _, f := range got {
			out = append(out, emittedFile{Path: f.Path, Content: f.Content})
		}
		return out, nil
	}
}

// compilecheckTree is one committed tree: the directory it lives in, the path
// prefix its emitters write under, and the emitters that fill it.
type compilecheckTree struct {
	dir      string
	prefix   string
	emitters []compilecheckEmitter
}

// Every emitter that produces source belongs to one of these trees. One added
// to a command and left out here emits code nothing compiles, which is what
// TestCompilecheckCoversEveryEmittedTree reports.
var compilecheckTrees = []compilecheckTree{
	{
		dir:    "compilecheck/server",
		prefix: "gen/go/server/",
		emitters: []compilecheckEmitter{
			{"EmitGoServer", goEmitter(EmitGoServer)},
			{"EmitCustomServer", goEmitter(EmitCustomServer)},
			{"EmitJobsHandlers", goEmitter(func(ir *dsl.IR, _ GenConfig) ([]GoFile, error) {
				return EmitJobsHandlers(ir)
			})},
		},
	},
	{
		dir:    "compilecheck/client",
		prefix: "clients/go/client/",
		emitters: []compilecheckEmitter{
			{"EmitGoClient", goEmitter(EmitGoClient)},
			{"EmitCustomClient", goEmitter(EmitCustomClient)},
			// The two jobs emitters differ only in their path and their jobs
			// import, and the import is the half a parse cannot check: one
			// names github.com/…/atlantis/jobs and the other
			// github.com/…/atlantis/clients/go/jobs, which are different
			// packages with separately declared Registry and Handler types.
			{"EmitCallerJobsHandlers", goEmitter(func(ir *dsl.IR, _ GenConfig) ([]GoFile, error) {
				return EmitCallerJobsHandlers(ir)
			})},
		},
	},
	{
		dir:    "compilecheck/keys",
		prefix: "gen/go/keys/",
		emitters: []compilecheckEmitter{
			{"EmitGoCacheKeys", goEmitter(func(ir *dsl.IR, _ GenConfig) ([]GoFile, error) {
				return EmitGoCacheKeys(ir)
			})},
		},
	},
	{
		// dir keeps the `atlantis` segment the prefix strips, so the tree on
		// disk is rooted the way the emitted imports expect.
		//
		// The Go trees can drop their prefix because compilecheckConfig
		// remaps the import path. Python has no equivalent: protoc derives a
		// module path from the proto path, so `from atlantis.library.v1
		// import book_pb2` resolves only under a directory literally called
		// atlantis. compilecheck/python is the source root; the _pb2 modules
		// are generated by buf and committed beside these, the arrangement
		// compilecheck/pb already has for Go.
		dir:    "compilecheck/python/atlantis",
		prefix: "atlantis/",
		emitters: []compilecheckEmitter{
			{"EmitPyClient", pyEmitter(func(ir *dsl.IR) ([]PyFile, error) {
				return EmitPyClient(ir, GenConfig{})
			})},
			{"EmitPyCustomClient", pyEmitter(EmitPyCustomClient)},
			{"EmitPyPackages", pyEmitter(EmitPyPackages)},
		},
	},
}

// emitCompilecheckTree renders one tree at the committed import paths, keyed by
// path relative to tree.dir.
func emitCompilecheckTree(t *testing.T, tree compilecheckTree, ir *dsl.IR) map[string]string {
	t.Helper()

	var files []emittedFile
	for _, em := range tree.emitters {
		got, err := em.fn(ir, compilecheckConfig)
		if err != nil {
			t.Fatalf("%s for compilecheck: %v", em.name, err)
		}
		files = append(files, got...)
	}

	out := map[string]string{}
	for _, gf := range files {
		rel, ok := strings.CutPrefix(gf.Path, tree.prefix)
		if !ok {
			t.Fatalf("emitted path %q does not start with %q; the remapping below "+
				"would put it somewhere unexpected", gf.Path, tree.prefix)
		}
		out[rel] = gf.Content
	}
	if len(out) == 0 {
		t.Fatalf("the emitters for %s produced nothing for the fixture", tree.dir)
	}
	return out
}

// compilecheckTreeFor returns the tree that compiles an emitted path, and the
// path relative to that tree's directory.
func compilecheckTreeFor(path string) (compilecheckTree, string, bool) {
	for _, tree := range compilecheckTrees {
		if rel, ok := strings.CutPrefix(path, tree.prefix); ok {
			return tree, rel, true
		}
	}
	return compilecheckTree{}, "", false
}

// compiledSuffixes are the emitted file extensions a compile fixture must
// claim.
//
// This list decides whether a file is checked at all, so an extension missing
// from it is emitted code that nothing type-checks and nothing reports. `.py`
// is here ahead of any Python emitter for that reason.
var compiledSuffixes = []string{".go", ".py"}

// Every source file the emitters produce must be compiled by one of the trees.
//
// The check is derived rather than listed: the golden set already holds every
// emitter's output for the fixture, so a golden file with no counterpart in a
// compile fixture is a hole, whatever emitter produced it.
func TestCompilecheckCoversEveryEmittedTree(t *testing.T) {
	goldenDir := filepath.Join("testdata", "golden")

	want, err := readGolden(goldenDir)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	checked := 0
	for path := range want {
		compiled := false
		for _, suffix := range compiledSuffixes {
			if strings.HasSuffix(path, suffix) {
				compiled = true
				break
			}
		}
		if !compiled {
			continue
		}
		tree, rel, ok := compilecheckTreeFor(path)
		if !ok {
			t.Errorf("%s is emitted but no compile fixture claims its prefix, so "+
				"it is never compiled. Add a tree to compilecheckTrees", path)
			continue
		}
		checked++
		if _, err := os.Stat(filepath.Join(tree.dir, rel)); err != nil {
			t.Errorf("%s is emitted but has no counterpart in %s, so it is never "+
				"compiled. Add its emitter to that tree: %v", path, tree.dir, err)
		}
	}
	if checked == 0 {
		t.Fatalf("the golden set holds no emitted %s files, so this test is "+
			"comparing two empty sets and proving nothing",
			strings.Join(compiledSuffixes, "/"))
	}
}

// producedByBuf reports whether a path in a compile fixture is protobuf output
// rather than emitter output.
//
// The Go trees keep their pb in compilecheck/pb, a directory no tree owns, so
// clearing a tree cannot reach it. The Python tree cannot be arranged that way:
// protoc derives a module path from the proto path, so its _pb2 modules have to
// sit inside the same `atlantis` package as the clients that import them.
// Clearing the directory wholesale therefore deletes them, and the next run
// type-checks a tree whose imports resolve to nothing.
func producedByBuf(path string) bool {
	for _, suffix := range []string{"_pb2.py", "_pb2.pyi", "_pb2_grpc.py", "_pb2_grpc.pyi"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// clearEmitted removes a tree's emitter-written files, leaving buf's alone.
func clearEmitted(t *testing.T, dir string) {
	t.Helper()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || producedByBuf(path) {
			return nil
		}
		return os.Remove(path)
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", dir, err)
	}
}

func writeCompilecheckTrees(t *testing.T, ir *dsl.IR) {
	t.Helper()
	for _, tree := range compilecheckTrees {
		clearEmitted(t, tree.dir)
		for rel, content := range emitCompilecheckTree(t, tree, ir) {
			dst := filepath.Join(tree.dir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dst, err)
			}
			if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", dst, err)
			}
		}
		t.Logf("rewrote %s", tree.dir)
	}
	t.Log("run `go build ./internal/codegen/compilecheck/...`, and regenerate " +
		"its pb with buf if the fixture's .proto changed")
}

func compareCompilecheckTrees(t *testing.T, ir *dsl.IR) {
	t.Helper()
	for _, tree := range compilecheckTrees {
		for rel, content := range emitCompilecheckTree(t, tree, ir) {
			path := filepath.Join(tree.dir, rel)
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
		entries, err := os.ReadDir(tree.dir)
		if err != nil {
			t.Fatalf("read %s: %v", tree.dir, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s is empty, so `go build` compiles nothing and the type "+
				"check it exists for is not happening", tree.dir)
		}
	}
}
