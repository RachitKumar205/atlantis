package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_RejectsUnsupportedVersion(t *testing.T) {
	p := writeManifest(t, `
version: 2
callers:
  - name: consumer
    source: git
    repo: file:///nope
    ref: main
    paths: [a.pc]
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestLoad_RejectsEmptyCallers(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers: []
`)
	if _, err := Load(p); err == nil {
		t.Fatal("want error on empty callers")
	}
}

func TestLoad_RejectsDuplicateCaller(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: file:///x
    ref: main
    paths: [a.pc]
  - name: consumer
    source: git
    repo: file:///y
    ref: main
    paths: [b.pc]
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestLoad_RejectsBadCallerName(t *testing.T) {
	for _, bad := range []string{"Consumer", "1consumer", "consumer-x", ""} {
		p := writeManifest(t, `
version: 1
callers:
  - name: `+quote(bad)+`
    source: git
    repo: file:///x
    ref: main
    paths: [a.pc]
`)
		if _, err := Load(p); err == nil {
			t.Errorf("want rejection of name %q", bad)
		}
	}
}

func TestLoad_RejectsUnknownSource(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: http
    repo: https://example.com
    ref: main
    paths: [a.pc]
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("want source error, got %v", err)
	}
}

func TestLoad_RejectsAbsoluteOrNonCanonicalPaths(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "a/../b.pc"} {
		p := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: file:///x
    ref: main
    paths: ["`+bad+`"]
`)
		if _, err := Load(p); err == nil {
			t.Errorf("want rejection of path %q", bad)
		}
	}
}

func TestLoad_AcceptsValid(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: file:///somewhere
    ref: main
    paths: [internal/auth/schema.pc, internal/cart/schema.pc]
  - name: vendor
    source: git
    repo: file:///elsewhere
    ref: v1.2.3
    paths: [internal/auth/schema.pc]
`)
	w, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(w.Callers) != 2 {
		t.Fatalf("want 2 callers, got %d", len(w.Callers))
	}
	if w.Path() != p {
		t.Errorf("path: got %s want %s", w.Path(), p)
	}
}

func TestResolve_ClonesAndReturnsAbsolutePaths(t *testing.T) {
	gitOrSkip(t)

	upstream := mkGitRepo(t, map[string]string{
		"internal/auth/schema.pc": "entity Account in consumer { id bigint primary }\n",
		"internal/cart/schema.pc": "entity Cart in consumer { id bigint primary }\n",
	})

	manifest := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: `+upstream+`
    ref: master
    paths: [internal/auth/schema.pc, internal/cart/schema.pc]
`)
	w, err := Load(manifest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	resolved, err := w.Resolve(cache)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Name != "consumer" {
		t.Fatalf("unexpected resolved set: %+v", resolved)
	}
	if len(resolved[0].Files) != 2 {
		t.Fatalf("want 2 files, got %d", len(resolved[0].Files))
	}
	for _, f := range resolved[0].Files {
		if !filepath.IsAbs(f) {
			t.Errorf("not absolute: %s", f)
		}
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing on disk: %v", err)
		}
	}
}

func TestResolve_ReusesCacheAcrossCalls(t *testing.T) {
	// A second Resolve against the same cache directory should not
	// re-clone from scratch; the .git directory survives.
	gitOrSkip(t)

	upstream := mkGitRepo(t, map[string]string{
		"a.pc": "entity A in consumer { id bigint primary }\n",
	})
	manifest := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: `+upstream+`
    ref: master
    paths: [a.pc]
`)
	w, _ := Load(manifest)
	cache := filepath.Join(t.TempDir(), "cache")
	if _, err := w.Resolve(cache); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Drop a sentinel inside the clone. A re-clone-from-scratch
	// would wipe it; a fetch-and-checkout leaves untracked files
	// alone. The sentinel surviving is the directly observable
	// "we reused the clone" signal.
	sentinel := filepath.Join(cache, "consumer", ".reuse-sentinel")
	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if _, err := w.Resolve(cache); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("clone was reinitialized; expected reuse (sentinel missing: %v)", err)
	}
}

func TestResolve_RejectsMissingPath(t *testing.T) {
	gitOrSkip(t)

	upstream := mkGitRepo(t, map[string]string{
		"present.pc": "entity A in x { id bigint primary }\n",
	})
	manifest := writeManifest(t, `
version: 1
callers:
  - name: consumer
    source: git
    repo: `+upstream+`
    ref: master
    paths: [missing.pc]
`)
	w, _ := Load(manifest)
	if _, err := w.Resolve(filepath.Join(t.TempDir(), "cache")); err == nil {
		t.Fatal("want error for missing path")
	}
}

// TestLoad_AcceptsLocalSource: the second supported source kind. Same
// validation surface as git but with path: instead of repo:/ref:.
func TestLoad_AcceptsLocalSource(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: backend
    source: local
    path: ../backend
    paths: [internal]
`)
	w, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(w.Callers) != 1 || w.Callers[0].Source != "local" || w.Callers[0].Path != "../backend" {
		t.Errorf("unexpected: %+v", w.Callers)
	}
}

// TestLoad_AcceptsMixedSources: nothing forces every caller to use the
// same source kind. A real adopter might pin one caller via git for
// reproducibility and run another via local for active iteration.
func TestLoad_AcceptsMixedSources(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: backend
    source: local
    path: ../backend
    paths: [internal]
  - name: vendor_platform
    source: git
    repo: file:///nowhere
    ref: main
    paths: [internal]
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestLoad_LocalRejectsRepoAndRef: a local caller that also carries
// git-only fields is almost certainly a typo (copy-pasted from a git
// caller and partially edited). Refuse to load so the operator notices.
func TestLoad_LocalRejectsRepoAndRef(t *testing.T) {
	cases := map[string]string{
		"with repo": `
version: 1
callers:
  - name: backend
    source: local
    path: ../backend
    repo: file:///x
    paths: [internal]
`,
		"with ref": `
version: 1
callers:
  - name: backend
    source: local
    path: ../backend
    ref: main
    paths: [internal]
`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Load(writeManifest(t, body)); err == nil {
				t.Errorf("want rejection")
			}
		})
	}
}

// TestLoad_LocalRequiresPath: source: local without a path: is
// unambiguously broken; reject at load time.
func TestLoad_LocalRequiresPath(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: backend
    source: local
    paths: [internal]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("want rejection")
	}
}

// TestLoad_GitRejectsPath: symmetric guard — source: git with path: set
// is a typo or paste error.
func TestLoad_GitRejectsPath(t *testing.T) {
	p := writeManifest(t, `
version: 1
callers:
  - name: backend
    source: git
    repo: file:///x
    ref: main
    path: ../backend
    paths: [internal]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("want rejection")
	}
}

// TestResolve_Local_RelativeToManifestDir: relative paths in the
// manifest resolve against the manifest's own directory, not the cwd
// where tidectl was invoked. That keeps `path: ../backend` stable
// regardless of where the operator runs the command from.
func TestResolve_Local_RelativeToManifestDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend", "internal"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// .atl file inside the caller's tree so the paths walk picks it up.
	if err := os.WriteFile(
		filepath.Join(root, "backend", "internal", "a.atl"),
		[]byte("entity A in x { id bigint primary }"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	manifestPath := filepath.Join(root, "atlantis", "atlantis.dev.yaml")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	body := `
version: 1
callers:
  - name: backend
    source: local
    path: ../backend
    paths: [internal]
`
	if err := os.WriteFile(manifestPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	w, err := Load(manifestPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolved, err := w.Resolve(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 1 || len(resolved[0].Files) != 1 {
		t.Fatalf("unexpected resolved: %+v", resolved)
	}
	if !filepath.IsAbs(resolved[0].Files[0]) {
		t.Errorf("not absolute: %s", resolved[0].Files[0])
	}
}

// TestResolve_Local_MissingPath: typo'd or stale path: surfaces as a
// load-time error, not a silent empty-resolution.
func TestResolve_Local_MissingPath(t *testing.T) {
	manifest := writeManifest(t, `
version: 1
callers:
  - name: backend
    source: local
    path: /this/does/not/exist
    paths: [internal]
`)
	w, _ := Load(manifest)
	if _, err := w.Resolve(filepath.Join(t.TempDir(), "cache")); err == nil {
		t.Fatal("want error for missing local path")
	}
}

// ---- test helpers ----

// writeManifest writes content to a fresh temp file and returns its path.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "atlantis.workspace.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

// gitOrSkip skips the test if `git` is not on PATH. CI environments are
// expected to have git; local runs without it (e.g. minimal containers)
// should not silently false-positive.
func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping integration test")
	}
}

// mkGitRepo creates a git repo under a fresh temp dir, writes each
// (path, contents) entry, commits, and returns the repo root as a
// file:// URL clone can consume.
func mkGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mustGit(t, root, "init", "--quiet")
	mustGit(t, root, "config", "user.email", "test@example.com")
	mustGit(t, root, "config", "user.name", "test")
	mustGit(t, root, "config", "commit.gpgsign", "false")
	for rel, content := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", abs, err)
		}
	}
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "--quiet", "-m", "test fixture")
	return "file://" + root
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// quote wraps a string for YAML embedding so a test case can pass an
// empty string or value containing quotes without escaping by hand.
func quote(s string) string { return "\"" + strings.ReplaceAll(s, `"`, `\"`) + "\"" }
