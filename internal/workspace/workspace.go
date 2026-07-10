// Package workspace loads an atlantis.workspace.yaml manifest and
// resolves the caller .atl files it pins.
//
// The manifest is the production authoritative for which caller
// schemas atlantis compiles into its typed Go surface. Each entry
// names a caller, points at a git repository, pins it to a ref
// (branch, tag, or full SHA), and lists the .atl paths inside the
// repository that contribute schema.
//
// Resolution clones each caller into a per-workspace cache, then
// returns absolute paths to the .atl files. Callers feed those paths to
// atlantis's existing schema-loading helpers; the rest of the
// codegen pipeline does not need to know whether the files came from
// a manifest or a local --schema-dir.
//
// Auth: this package shells out to `git`; whatever credentials the
// surrounding environment provides (SSH agent, HTTPS PAT, gh CLI) are
// inherited. No credential handling lives here.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"

	"gopkg.in/yaml.v3"
)

// Workspace is the parsed contents of an atlantis.workspace.yaml file.
//
// The wire shape is narrow: two supported source kinds
// (git for prod, local for dev), a flat list of callers, no inheritance
// or includes. New kinds extend Source rather than growing the
// top-level shape.
type Workspace struct {
	Version int      `yaml:"version"`
	Callers []Caller `yaml:"callers"`

	// path remembers where Load read the manifest from, for error
	// messages and so Resolve can compute cache directories relative
	// to the manifest if asked.
	path string
}

// Caller is one row of the manifest.
type Caller struct {
	// Name is the IR namespace this caller owns. Must be a valid
	// identifier so it survives proto / Go codegen unchanged.
	Name string `yaml:"name"`

	// Source selects the fetcher. Supported values: "git" (production:
	// pinned ref clone), "local" (development: filesystem path, working
	// tree). Validation rejects anything else so a typo doesn't
	// silently fall through to a no-op fetcher.
	Source string `yaml:"source"`

	// Repo is the URL git understands: https://, git@, file:// or a
	// bare filesystem path (used by the test suite). Required when
	// source is "git"; rejected otherwise.
	Repo string `yaml:"repo,omitempty"`

	// Ref is the branch, tag, or full SHA to check out. We do not
	// default to HEAD: pinning an explicit ref is the whole point of
	// the manifest. Required when source is "git"; rejected otherwise.
	Ref string `yaml:"ref,omitempty"`

	// Path is the filesystem location of the caller's working tree.
	// Required when source is "local"; rejected otherwise. Relative
	// paths resolve against the manifest's own directory so
	// `path: ../api` works regardless of where the operator
	// invokes tidectl from.
	Path string `yaml:"path,omitempty"`

	// Paths are the .atl file paths inside the caller repository,
	// relative to its root. We never glob — every contributing file
	// is listed explicitly so the manifest is auditable.
	Paths []string `yaml:"paths"`
}

// ResolvedCaller is what Resolve returns: a caller plus absolute paths
// to its .atl files on the local filesystem (already fetched).
type ResolvedCaller struct {
	Name      string
	CloneRoot string   // absolute path to the cloned caller tree
	Files     []string // absolute paths to the .atl files
}

// Load parses the manifest at path. Validation rejects malformed
// manifests up front so Resolve never has to defend against partial
// data. The validation errors aim to be precise enough for a CI log
// to be self-explanatory.
func Load(path string) (*Workspace, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var w Workspace
	if err := yaml.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	w.path = path
	if err := w.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &w, nil
}

// Path returns the manifest path Load read from.
func (w *Workspace) Path() string { return w.path }

// Resolve fetches each caller into cacheDir/<caller>/ and returns the
// absolute paths to its .atl files. The cache is keyed by caller name
// only; refreshing to a new ref reuses the directory. Concurrent
// callers across distinct Workspace instances must use disjoint cache
// directories — there is no in-process locking.
//
// Resolve always shells out to `git fetch` so a moving branch like
// "main" picks up new commits between runs. A pinned SHA short-
// circuits the fetch when the local HEAD already matches.
func (w *Workspace) Resolve(cacheDir string) ([]*ResolvedCaller, error) {
	if cacheDir == "" {
		return nil, errors.New("workspace: cacheDir is required")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	out := make([]*ResolvedCaller, 0, len(w.Callers))
	for _, c := range w.Callers {
		root, err := w.resolveCaller(c, cacheDir)
		if err != nil {
			return nil, fmt.Errorf("caller %s: %w", c.Name, err)
		}
		files := make([]string, 0, len(c.Paths))
		for _, rel := range c.Paths {
			abs := filepath.Join(root, rel)
			if _, err := os.Stat(abs); err != nil {
				return nil, fmt.Errorf("caller %s: path %s missing in clone: %w", c.Name, rel, err)
			}
			files = append(files, abs)
		}
		// Deterministic ordering survives across reruns so downstream
		// IR hashing isn't sensitive to manifest column order.
		sort.Strings(files)
		out = append(out, &ResolvedCaller{
			Name:      c.Name,
			CloneRoot: root,
			Files:     files,
		})
	}
	return out, nil
}

// validate runs cheap structural checks on the parsed manifest. It is
// strict by design: a typo in the YAML is far less expensive to fix
// here than after the manifest has been merged.
func (w *Workspace) validate() error {
	if w.Version != 1 {
		return fmt.Errorf("unsupported version %d (want 1)", w.Version)
	}
	if len(w.Callers) == 0 {
		return errors.New("no callers declared")
	}
	seen := make(map[string]struct{}, len(w.Callers))
	for i, c := range w.Callers {
		if !callerNameRe.MatchString(c.Name) {
			return fmt.Errorf("caller[%d]: name %q must match %s", i, c.Name, callerNameRe.String())
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("caller %q declared more than once", c.Name)
		}
		seen[c.Name] = struct{}{}

		switch c.Source {
		case "git":
			if c.Repo == "" {
				return fmt.Errorf("caller %s: repo is required for source: git", c.Name)
			}
			if c.Ref == "" {
				return fmt.Errorf("caller %s: ref is required for source: git", c.Name)
			}
			if c.Path != "" {
				return fmt.Errorf("caller %s: path is not allowed for source: git (use source: local)", c.Name)
			}
		case "local":
			if c.Path == "" {
				return fmt.Errorf("caller %s: path is required for source: local", c.Name)
			}
			if c.Repo != "" {
				return fmt.Errorf("caller %s: repo is not allowed for source: local", c.Name)
			}
			if c.Ref != "" {
				return fmt.Errorf("caller %s: ref is not allowed for source: local", c.Name)
			}
		default:
			return fmt.Errorf("caller %s: source %q is not supported (want \"git\" or \"local\")", c.Name, c.Source)
		}
		if len(c.Paths) == 0 {
			return fmt.Errorf("caller %s: at least one path is required", c.Name)
		}
		for _, p := range c.Paths {
			if filepath.IsAbs(p) {
				return fmt.Errorf("caller %s: path %q must be relative to the repo root", c.Name, p)
			}
			if clean := filepath.Clean(p); clean != p {
				return fmt.Errorf("caller %s: path %q must be in canonical form (got %q)", c.Name, p, clean)
			}
		}
	}
	return nil
}

// callerNameRe restricts caller names to characters safe to use as a
// proto/Go package fragment and as a filesystem path segment. Same
// shape the codegen already assumes for IR namespaces.
var callerNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// resolveCaller dispatches to the right fetcher for the source kind.
// Source-specific validation happens at Load time; resolveCaller just
// routes.
func (w *Workspace) resolveCaller(c Caller, cacheDir string) (string, error) {
	switch c.Source {
	case "git":
		return fetchGit(cacheDir, c)
	case "local":
		return w.resolveLocal(c)
	}
	// Unreachable: validate() rejects other sources at Load time.
	return "", fmt.Errorf("internal: unsupported source %q passed validation", c.Source)
}

// resolveLocal returns the absolute path to the caller's working tree.
// Relative paths in the manifest resolve against the manifest's own
// directory so `path: ../api` is stable across invocations from
// different working directories. The directory must exist; we don't
// auto-create it because that would mask typos.
func (w *Workspace) resolveLocal(c Caller) (string, error) {
	p := c.Path
	if !filepath.IsAbs(p) {
		// w.path may itself be relative if Load was called with a
		// relative argument. Absolute-ify both for predictable joins.
		manifestAbs, err := filepath.Abs(w.path)
		if err != nil {
			return "", fmt.Errorf("manifest abs: %w", err)
		}
		p = filepath.Join(filepath.Dir(manifestAbs), p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("local path %s: %w", c.Path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("local path %s is not a directory", c.Path)
	}
	return p, nil
}

// fetchGit ensures the caller's repository is checked out at the
// pinned ref under cacheDir/<caller>/, then returns that path.
//
// Strategy:
//   - First call: clone into the destination.
//   - Subsequent calls: fetch + checkout, so a moving branch ref
//     picks up new commits.
//   - Repo URL change: re-init from scratch (the user changed the
//     upstream; the old clone's history is irrelevant).
func fetchGit(cacheDir string, c Caller) (string, error) {
	dst := filepath.Join(cacheDir, c.Name)
	cloneURL, err := getRemoteURL(dst)
	if err != nil {
		// Not a git checkout, or doesn't exist. Re-clone fresh.
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("clear stale clone: %w", err)
		}
		if err := runGit("", "clone", "--no-tags", c.Repo, dst); err != nil {
			return "", fmt.Errorf("clone %s: %w", c.Repo, err)
		}
	} else if cloneURL != c.Repo {
		// Repo URL changed in the manifest; we can't reuse the
		// existing clone safely.
		if err := os.RemoveAll(dst); err != nil {
			return "", fmt.Errorf("clear repo-changed clone: %w", err)
		}
		if err := runGit("", "clone", "--no-tags", c.Repo, dst); err != nil {
			return "", fmt.Errorf("re-clone %s: %w", c.Repo, err)
		}
	}

	// Fetch is cheap when the ref hasn't moved; running it
	// unconditionally avoids the cache-staleness pitfall.
	if err := runGit(dst, "fetch", "--no-tags", "origin", c.Ref); err != nil {
		return "", fmt.Errorf("fetch %s: %w", c.Ref, err)
	}
	// FETCH_HEAD is what `fetch origin <ref>` writes; checking out
	// the symbol is what tracks a moving branch or a pinned SHA
	// equivalently.
	if err := runGit(dst, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("checkout %s: %w", c.Ref, err)
	}
	return dst, nil
}

// getRemoteURL returns the configured origin URL for an existing
// clone, or a non-nil error if the path is not a git checkout.
func getRemoteURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return trimRight(string(out)), nil
}

// runGit executes a git subcommand with cwd at dir (empty = the
// caller's cwd). stdout is dropped; stderr propagates into the
// returned error so failures are diagnostic.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w\n%s", args, err, out)
	}
	return nil
}

// trimRight strips a trailing newline from `git remote get-url` output.
func trimRight(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
