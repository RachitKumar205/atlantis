package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMirrorFiles_WritesUnderCallerRoot(t *testing.T) {
	root := t.TempDir()
	files := []SubmittedFile{
		{Path: "internal/auth/schema.pc", Content: []byte("entity A in x { id bigint primary }\n")},
		{Path: "internal/cart/schema.pc", Content: []byte("entity B in x { id bigint primary }\n")},
	}
	if err := mirrorFiles(root, "consumer", files); err != nil {
		t.Fatalf("mirrorFiles: %v", err)
	}
	for _, f := range files {
		dst := filepath.Join(root, "consumer", f.Path)
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read %s: %v", dst, err)
		}
		if string(got) != string(f.Content) {
			t.Errorf("%s: contents drift", dst)
		}
	}
}

func TestMirrorFiles_IsIdempotentOnIdenticalContent(t *testing.T) {
	// A second mirror of the same bytes must not bump mtime — a watcher
	// would otherwise re-trigger codegen every apply even when the schema
	// hasn't moved.
	root := t.TempDir()
	files := []SubmittedFile{{Path: "x.pc", Content: []byte("entity A in x { id bigint primary }\n")}}

	if err := mirrorFiles(root, "consumer", files); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	dst := filepath.Join(root, "consumer", "x.pc")
	first, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after first: %v", err)
	}

	if err := mirrorFiles(root, "consumer", files); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	second, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat after second: %v", err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Errorf("mtime drifted on identical-content rewrite: %v -> %v",
			first.ModTime(), second.ModTime())
	}
}

func TestMirrorFiles_RewritesOnContentChange(t *testing.T) {
	root := t.TempDir()
	files := []SubmittedFile{{Path: "x.pc", Content: []byte("v1\n")}}
	if err := mirrorFiles(root, "consumer", files); err != nil {
		t.Fatalf("first: %v", err)
	}
	files[0].Content = []byte("v2\n")
	if err := mirrorFiles(root, "consumer", files); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "consumer", "x.pc"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "v2\n" {
		t.Errorf("content not refreshed: %q", got)
	}
}

func TestMirrorFiles_PartitionsByCaller(t *testing.T) {
	// Two callers may submit files whose paths happen to collide; the
	// per-caller subdirectory keeps them apart.
	root := t.TempDir()
	if err := mirrorFiles(root, "consumer", []SubmittedFile{
		{Path: "internal/auth/schema.pc", Content: []byte("consumer\n")},
	}); err != nil {
		t.Fatalf("consumer: %v", err)
	}
	if err := mirrorFiles(root, "vendor", []SubmittedFile{
		{Path: "internal/auth/schema.pc", Content: []byte("vendor\n")},
	}); err != nil {
		t.Fatalf("vendor: %v", err)
	}
	for caller, want := range map[string]string{"consumer": "consumer\n", "vendor": "vendor\n"} {
		got, err := os.ReadFile(filepath.Join(root, caller, "internal/auth/schema.pc"))
		if err != nil {
			t.Fatalf("read %s: %v", caller, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %q want %q", caller, got, want)
		}
	}
}

func TestMirrorFiles_RejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"../escape.pc",
		"a/../../escape.pc",
		"/absolute.pc",
		".",
	}
	for _, p := range cases {
		err := mirrorFiles(root, "consumer", []SubmittedFile{{Path: p, Content: []byte("x")}})
		if err == nil {
			t.Errorf("%q: expected rejection, got nil", p)
		}
	}
}

func TestMirrorFiles_RejectsEmptyArgs(t *testing.T) {
	if err := mirrorFiles("", "consumer", nil); err == nil {
		t.Error("empty root: want error")
	}
	if err := mirrorFiles(t.TempDir(), "", nil); err == nil {
		t.Error("empty caller: want error")
	}
}

func TestMirrorFiles_LeavesNoTempArtifactOnSuccess(t *testing.T) {
	// The atomic-rename pattern uses CreateTemp; a successful write
	// must rename the temp into place rather than leaving it behind.
	root := t.TempDir()
	if err := mirrorFiles(root, "consumer", []SubmittedFile{
		{Path: "x.pc", Content: []byte("ok\n")},
	}); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "consumer"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pc-mirror-") {
			t.Errorf("leftover temp artifact: %s", e.Name())
		}
	}
}
