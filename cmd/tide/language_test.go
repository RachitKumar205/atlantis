package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func TestResolveLanguage(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    sdkLanguage
		wantErr bool
	}{
		// Absent means Go. Every tide.yaml written before Python existed
		// carries no `language:`, and each must keep generating what it did.
		{in: "", want: langGo},
		{in: "go", want: langGo},
		{in: "python", want: langPython},
		// Trimmed and case-folded: the value is hand-written in YAML and can
		// arrive from ATL_LANGUAGE.
		{in: "  Python ", want: langPython},
		{in: "GO", want: langGo},
		{in: "golang", wantErr: true},
		{in: "py", wantErr: true},
		{in: "rust", wantErr: true},
	} {
		got, err := resolveLanguage(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveLanguage(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveLanguage(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveLanguage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUnknownLanguageNamesTheAcceptedOnes(t *testing.T) {
	_, err := resolveLanguage("ruby")
	if err == nil {
		t.Fatal("an unknown language was accepted")
	}
	// Generating Go for a caller who asked for something else writes a whole
	// tree in the wrong language and reports success, so the error has to say
	// what it will accept.
	for _, want := range []string{"ruby", "go", "python"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Python's buf output lands inside atlantis/, so `atlantis` is the whole owned
// set. Listing `pb` and `client` as well would delete nothing (they never
// exist) but would let a caller's own `client/` directory be swept.
func TestOwnedRootsPerLanguage(t *testing.T) {
	if got := langGo.ownedRoots(); !slices.Equal(got, []string{"atlantis", "pb", "client"}) {
		t.Errorf("go owned roots = %v", got)
	}
	if got := langPython.ownedRoots(); !slices.Equal(got, []string{"atlantis"}) {
		t.Errorf("python owned roots = %v, want just atlantis", got)
	}
}

// go.mod is Go's business. A Python repository has none, and reading it anyway
// fails before anything is generated, naming a file that repository has no
// reason to contain.
func TestOnlyGoNeedsTheGoModule(t *testing.T) {
	if !langGo.needsGoModule() {
		t.Error("go does not ask for go.mod, so the emitted import prefix has no source")
	}
	if langPython.needsGoModule() {
		t.Error("python asks for go.mod; `tide generate` would fail in a repository that has none")
	}
}

// The two languages plan different files from the same protos.
func TestPlanSDKFilesPerLanguage(t *testing.T) {
	ir := planLanguageFixture(t)

	goFiles, err := planSDKFiles(ir, langGo, "example.com/app/gen")
	if err != nil {
		t.Fatalf("plan go: %v", err)
	}
	pyFiles, err := planSDKFiles(ir, langPython, "")
	if err != nil {
		t.Fatalf("plan python: %v", err)
	}

	// The protos are the wire contract and do not vary by language.
	for path, content := range goFiles {
		if !strings.HasSuffix(path, ".proto") {
			continue
		}
		if pyFiles[path] != content {
			t.Errorf("%s differs between the two languages; it is the wire contract "+
				"and only the plugins buf runs over it should differ", path)
		}
	}

	if _, ok := goFiles["client/library/book_client.go"]; !ok {
		t.Error("the Go plan has no client/library/book_client.go")
	}
	if _, ok := pyFiles["atlantis/library/v1/book_client.py"]; !ok {
		t.Error("the Python plan has no atlantis/library/v1/book_client.py")
	}
	// No crossing over: a Go file in the Python plan would be written into the
	// caller's tree and compile nowhere.
	for path := range pyFiles {
		if strings.HasSuffix(path, ".go") {
			t.Errorf("the Python plan holds a Go file: %s", path)
		}
	}
	for path := range goFiles {
		if strings.HasSuffix(path, ".py") {
			t.Errorf("the Go plan holds a Python file: %s", path)
		}
	}
}

// The Python buf config must not carry the Go plugin's settings, and must
// carry the two mypy plugins.
func TestPythonBufConfig(t *testing.T) {
	gen, yaml := bufConfigs(langPython, "")
	if yaml != "version: v2\n" {
		t.Errorf("buf.yaml = %q", yaml)
	}
	// Measured: without the mypy plugins, `mypy --strict` over the generated
	// tree reports 186 errors, every one inside a generated stub.
	for _, want := range []string{
		"buf.build/protocolbuffers/python",
		"buf.build/grpc/python",
		"buf.build/community/nipunn1313-mypy",
		"buf.build/community/nipunn1313-mypy-grpc",
	} {
		if !strings.Contains(gen, want) {
			t.Errorf("the python buf.gen.yaml does not run %s", want)
		}
	}
	// protoc derives a module path from the proto path, so the output has to
	// land at the root. `out: pb` would produce pb/atlantis/... and
	// `from atlantis.library.v1 import book_pb2` would resolve to nothing.
	if !strings.Contains(gen, "out: .") {
		t.Error("the python buf.gen.yaml does not write to the source root")
	}
	for _, unwanted := range []string{"managed:", "go_package_prefix", "paths=source_relative"} {
		if strings.Contains(gen, unwanted) {
			t.Errorf("the python buf.gen.yaml carries %q, which configures the Go plugin", unwanted)
		}
	}
}

// A tide.yaml with no `language:` must still produce exactly the Go tree.
//
// This is the whole compatibility claim of the change: an existing caller
// regenerating gets a byte-identical result.
func TestAbsentLanguageGeneratesTheGoTree(t *testing.T) {
	ir := planLanguageFixture(t)

	lang, err := resolveLanguage("")
	if err != nil {
		t.Fatalf("resolveLanguage(\"\"): %v", err)
	}
	viaDefault, err := planSDKFiles(ir, lang, "example.com/app/gen")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	explicit, err := planSDKFiles(ir, langGo, "example.com/app/gen")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(viaDefault) != len(explicit) {
		t.Fatalf("absent language planned %d files, explicit go planned %d",
			len(viaDefault), len(explicit))
	}
	for path, content := range explicit {
		if viaDefault[path] != content {
			t.Errorf("%s differs between an absent `language:` and an explicit one", path)
		}
	}
}

// A Go manifest records no language, so regenerating an untouched Go tree
// produces no diff — including in the manifest itself.
func TestGoManifestRecordsNoLanguage(t *testing.T) {
	hashes := map[string]string{"client/library/book_client.go": "abc"}
	goM := buildManifest("api", "example.com/app/gen", "v1", langGo, []string{"library"}, hashes)
	if goM.Language != "" {
		t.Errorf("a Go manifest records language %q; it should be absent so the file "+
			"is identical to one written before the field existed", goM.Language)
	}
	if goM.language() != langGo {
		t.Errorf("an absent language reads as %q, want go", goM.language())
	}

	pyM := buildManifest("api", "", "v1", langPython, []string{"library"}, hashes)
	if pyM.Language != string(langPython) {
		t.Errorf("a Python manifest records language %q, want python", pyM.Language)
	}
}

// Regenerating in a different language is reported as stale, not as a tree
// that cannot be understood: `tide generate` fixes it.
func TestCheckGeneratedReportsALanguageSwitch(t *testing.T) {
	m := buildManifest("api", "example.com/app/gen", "v1", langGo,
		[]string{"library"}, map[string]string{"client/library/book_client.go": "abc"})

	got := checkGenerated(&m, manifestHashes(&m), "api", "example.com/app/gen", langPython, nil)
	if got.Code != checkStale {
		t.Fatalf("switching language gave code %d, want %d (stale)", got.Code, checkStale)
	}
	if !strings.Contains(got.Message, "go") || !strings.Contains(got.Message, "python") {
		t.Errorf("message %q names neither the committed language nor the requested one", got.Message)
	}
}

func planLanguageFixture(t *testing.T) *dsl.IR {
	t.Helper()
	src := `
entity Book in library {
  id     bigint primary
  title  text not null
}
`
	f, err := dsl.Parse("fixture.atl", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}
