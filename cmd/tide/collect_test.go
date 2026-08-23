package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What collectPCFiles must not pick up.
//
// The failure this guards: `tide plan` runs a pull first, which writes the
// organisation's merged schema into .tide-cache/schema/. With the documented
// `schema_paths: ["."]` the next plan submitted the caller's own entities
// twice and the server refused with "duplicate entity app.Note (first declared
// at backend:.tide-cache/schema/schema.atl)" — a path the author never wrote.
//
// The shape that makes it expensive: the first apply in a fresh workspace
// works, and every one after it fails.
func TestTheSchemaWalkerSkipsHiddenDirectories(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("schema.atl", "entity Note in app { id bigint primary }\n")
	write("sub/more.atl", "entity Tag in app { id bigint primary }\n")
	// What `tide pull` leaves behind, and what broke this.
	write(".tide-cache/schema/schema.atl", "entity Note in app { id bigint primary }\n")
	// Two more that are not schema and were never meant to be read.
	write(".git/objects/stray.atl", "entity Junk in app { id bigint primary }\n")
	write(".venv/lib/vendored.atl", "entity Vendored in app { id bigint primary }\n")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// Exactly the documented configuration, which is the one that broke.
	files, err := collectPCFiles([]string{"."})
	if err != nil {
		t.Fatalf("collectPCFiles: %v", err)
	}

	got := map[string]bool{}
	for _, f := range files {
		got[filepath.ToSlash(f.GetPath())] = true
	}
	for _, want := range []string{"schema.atl", "sub/more.atl"} {
		if !got[want] {
			t.Errorf("%s was not collected; found %v", want, keys(got))
		}
	}
	for p := range got {
		if strings.HasPrefix(p, ".") && p != "." {
			t.Errorf("collected %q from a hidden directory", p)
		}
	}
	if len(files) != 2 {
		t.Errorf("collected %d files, want 2: %v", len(files), keys(got))
	}
}

// A hidden directory named as a schema path explicitly is still read.
//
// Skipping is about walking into hidden directories, not about refusing a path
// somebody deliberately configured. Without this the fix would silently ignore
// a caller whose schema really does live somewhere hidden.
func TestAHiddenDirectoryNamedExplicitlyIsStillRead(t *testing.T) {
	dir := t.TempDir()
	hidden := filepath.Join(dir, ".schema")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "s.atl"),
		[]byte("entity Note in app { id bigint primary }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := collectPCFiles([]string{hidden})
	if err != nil {
		t.Fatalf("collectPCFiles: %v", err)
	}
	if len(files) != 1 {
		t.Errorf("collected %d files from an explicitly named hidden path, want 1", len(files))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
