package codegen

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

var updateGolden = flag.Bool("update", false,
	"rewrite internal/codegen/testdata/golden from the current emitters")

// Every emitter's output, pinned against a committed fixture.
//
// `make codegen-check` cannot catch emitter drift: it runs `tidectl codegen`
// into a temp dir and diffs against the checked-in `gen/`, and this repo ships
// no `.atl` files, since callers keep their schemas in their own repos. On a CI
// checkout the command emits nothing, `gen/` is absent and gitignored, `mkdir
// -p` creates both sides empty, and the diff succeeds for any emitter change at
// all. The same gate fails on a developer machine holding generated output from
// a schema that has since moved away.
//
// testdata/schema.atl is committed, so the emitters always have real input, and
// the expected output is committed beside it.
//
// Golden files rather than substring assertions: the rest of this package
// asserts that a particular line appears, which answers "is this guard
// emitted?" and not "did anything else move?".
func TestEmittersMatchGolden(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "schema.atl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f, err := dsl.Parse("testdata/schema.atl", src)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatalf("lower fixture: %v", err)
	}
	// Mirrors cmd/tidectl/codegen.go: proto numbers are assigned before any
	// emitter runs, and every emitter reads them.
	AssignProtoNumbers(nil, ir)

	got := map[string]string{}

	// The same emitter set cmd/tidectl/codegen.go drives. A new emitter added
	// there and not here is the one gap this test cannot see, which is what
	// TestGoldenCoversEveryEmitter below is for.
	goEmitters := []struct {
		name string
		fn   func() ([]GoFile, error)
	}{
		{"go server", func() ([]GoFile, error) { return EmitGoServer(ir, GenConfig{}) }},
		{"go client", func() ([]GoFile, error) { return EmitGoClient(ir, GenConfig{}) }},
		{"go keys", func() ([]GoFile, error) { return EmitGoCacheKeys(ir) }},
		{"go custom server", func() ([]GoFile, error) { return EmitCustomServer(ir, GenConfig{}) }},
		{"go custom client", func() ([]GoFile, error) { return EmitCustomClient(ir, GenConfig{}) }},
		{"go jobs handlers", func() ([]GoFile, error) { return EmitJobsHandlers(ir) }},
		{"go workflows", func() ([]GoFile, error) { return EmitWorkflows(ir) }},
		{"go ephemerals", func() ([]GoFile, error) { return EmitEphemerals(ir) }},
	}
	for _, e := range goEmitters {
		files, ferr := e.fn()
		if ferr != nil {
			t.Fatalf("%s: %v", e.name, ferr)
		}
		for _, gf := range files {
			got[gf.Path] = gf.Content
		}
	}

	protos, err := EmitProto(ir)
	if err != nil {
		t.Fatalf("EmitProto: %v", err)
	}
	customProtos, err := EmitCustomProto(ir)
	if err != nil {
		t.Fatalf("EmitCustomProto: %v", err)
	}
	for _, pf := range append(protos, customProtos...) {
		got[pf.Path] = pf.Content
	}

	// The initial DDL too. It is the artifact that actually touches a
	// customer's data, so an unreviewed change to it is the most expensive
	// kind this test can catch.
	sqlScripts, err := EmitInitial(ir)
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	got["sql/initial.up.sql"] = sqlScripts.Up
	got["sql/initial.down.sql"] = sqlScripts.Down

	if len(got) == 0 {
		t.Fatal("the emitters produced nothing from the fixture, so this test " +
			"would pass against any implementation — the exact failure mode it " +
			"was written to replace")
	}

	goldenDir := filepath.Join("testdata", "golden")
	if *updateGolden {
		if rerr := os.RemoveAll(goldenDir); rerr != nil {
			t.Fatalf("clear golden dir: %v", rerr)
		}
		for path, content := range got {
			dst := filepath.Join(goldenDir, path)
			if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
				t.Fatalf("mkdir %s: %v", dst, merr)
			}
			if werr := os.WriteFile(dst, []byte(content), 0o644); werr != nil {
				t.Fatalf("write %s: %v", dst, werr)
			}
		}
		t.Logf("wrote %d golden files; review the diff before committing", len(got))
		writeCompilecheckServer(t, ir)
		return
	}
	compareCompilecheckServer(t, ir)

	want, err := readGolden(goldenDir)
	if err != nil {
		t.Fatalf("read golden: %v\nRun: go test ./internal/codegen -run Golden -update", err)
	}
	if len(want) == 0 {
		t.Fatal("no golden files on disk. An empty expectation set makes every " +
			"comparison below vacuous.\n" +
			"Run: go test ./internal/codegen -run Golden -update")
	}

	for _, path := range sortedPaths(got) {
		w, ok := want[path]
		if !ok {
			t.Errorf("%s: emitted but has no golden file. A new artifact reaches "+
				"every caller's repo on their next codegen run, so it should be "+
				"reviewed here first", path)
			continue
		}
		if got[path] != w {
			t.Errorf("%s: emitted output differs from the golden file.\n%s",
				path, firstDifference(w, got[path]))
		}
	}
	for _, path := range sortedPaths(want) {
		if _, ok := got[path]; !ok {
			t.Errorf("%s: has a golden file but is no longer emitted. If the "+
				"artifact was deliberately dropped, callers lose it on their next "+
				"codegen run", path)
		}
	}
}

// The golden set has to cover every emitter cmd/tidectl/codegen.go runs.
//
// TestEmittersMatchGolden lists the emitters by hand, so an emitter added to the
// command and not to the list is invisible to it — the golden files would stay
// green while an entire artifact went unreviewed. This reads the command's
// source and checks the two lists agree.
func TestGoldenCoversEveryEmitter(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "cmd", "tidectl", "codegen.go"))
	if err != nil {
		t.Fatalf("read codegen.go: %v", err)
	}
	golden, err := os.ReadFile("golden_test.go")
	if err != nil {
		t.Fatalf("read golden_test.go: %v", err)
	}

	// Every codegen.EmitX referenced by the command.
	var missing []string
	for _, line := range strings.Split(string(src), "\n") {
		idx := strings.Index(line, "codegen.Emit")
		if idx < 0 {
			continue
		}
		rest := line[idx+len("codegen."):]
		end := strings.IndexAny(rest, "(")
		if end < 0 {
			continue
		}
		name := rest[:end]
		if !strings.Contains(string(golden), name+"(ir") {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	missing = dedupeStrings(missing)
	if len(missing) > 0 {
		t.Errorf("cmd/tidectl/codegen.go runs these emitters but "+
			"TestEmittersMatchGolden does not, so their output is never "+
			"reviewed: %s", strings.Join(missing, ", "))
	}
}

func readGolden(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}

func sortedPaths(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedupeStrings(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// firstDifference reports the first differing line with a little context.
// A full diff of a many-hundred-line emitted file buries the change; the first
// divergence is almost always the one worth reading.
func firstDifference(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w == g {
			continue
		}
		var b strings.Builder
		for j := max(0, i-2); j < i; j++ {
			b.WriteString("      " + wl[j] + "\n")
		}
		b.WriteString("want: " + w + "\n")
		b.WriteString("got:  " + g + "\n")
		b.WriteString("(line " + itoa(i+1) + "; " +
			itoa(len(wl)) + " golden lines vs " + itoa(len(gl)) + " emitted)")
		return b.String()
	}
	return "(files differ only in trailing content)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
