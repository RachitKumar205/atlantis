package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// generateManifestName is the record `tide generate` leaves beside the code it
// wrote. Committed with that code: it is what `--check` compares against, so a
// checkout without it cannot be verified.
const generateManifestName = "tide.manifest.json"

// manifestSchema is the version of this file's own shape. A manifest written
// by a later tide is refused rather than misread.
const manifestSchema = 1

// generateManifest records what `tide generate` produced.
//
// Files carries a hash per generated path, which is what lets --check run with
// no server, no credentials and no buf: the expected value is on disk rather
// than recomputed.
//
// Three values are deliberately absent, each of which would differ between two
// runs that produced identical code:
//
//   - the checkpoint content hash, which covers every caller's schema merged
//     together and moves when an unrelated caller applies
//   - a generation timestamp
//   - anything derived from the toolchain that produced the .pb.go files
//
// TideVersion is recorded and never compared. main.go defaults it to "dev", so
// a release binary and `go run ./cmd/tide` disagree for ever.
type generateManifest struct {
	Schema       int            `json:"schema"`
	Caller       string         `json:"caller"`
	Namespaces   []string       `json:"namespaces"`
	ModulePrefix string         `json:"module_prefix"`
	TideVersion  string         `json:"tide_version"`
	Files        []manifestFile `json:"files"`
}

type manifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ownedRoots are the directories generate writes and may therefore remove.
// Anything else under output_dir belongs to the caller.
func ownedRoots() []string { return []string{"atlantis", "pb", "client"} }

// ownedFiles are the files generate writes at the top of output_dir. The
// manifest is not among them: it records the others and cannot record itself.
func ownedFiles() []string { return []string{"buf.gen.yaml", "buf.yaml"} }

// buildManifest assembles the record from a hashed tree.
//
// Namespaces and Files are sorted so the file is stable across runs whatever
// order the caller listed namespaces in or the walk returned paths.
func buildManifest(caller, modulePrefix, tideVersion string, namespaces []string, hashes map[string]string) generateManifest {
	ns := append([]string(nil), namespaces...)
	sort.Strings(ns)

	files := make([]manifestFile, 0, len(hashes))
	for p, h := range hashes {
		files = append(files, manifestFile{Path: p, SHA256: h})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return generateManifest{
		Schema:       manifestSchema,
		Caller:       caller,
		Namespaces:   ns,
		ModulePrefix: modulePrefix,
		TideVersion:  tideVersion,
		Files:        files,
	}
}

// readManifest loads the record from outDir.
//
// Every failure is reported. readCachedVersion swallows its errors because an
// unreadable cache costs a re-fetch; an unreadable manifest here would make
// --check pass on a tree it never verified, which is the vacuous gate this
// exists to avoid.
func readManifest(outDir string) (*generateManifest, error) {
	path := filepath.Join(outDir, generateManifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m generateManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Schema != manifestSchema {
		return nil, fmt.Errorf("%s: manifest schema %d, this tide understands %d",
			path, m.Schema, manifestSchema)
	}
	return &m, nil
}

// writeManifest replaces the record, via a temporary file in the same
// directory. The same rename credstore uses for the client key: a partial
// manifest would describe a tree that was never produced.
func writeManifest(outDir string, m generateManifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	final := filepath.Join(outDir, generateManifestName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// hashOwnedTree hashes every file generate owns under outDir, keyed by a
// slash-separated path relative to it.
//
// A missing owned root is not an error: a schema with no custom queries emits
// no client package, and a tree that was never generated has none of them.
func hashOwnedTree(outDir string) (map[string]string, error) {
	out := map[string]string{}

	for _, root := range ownedRoots() {
		dir := filepath.Join(outDir, root)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(outDir, path)
			if err != nil {
				return err
			}
			h, err := hashFile(path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = h
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	for _, name := range ownedFiles() {
		h, err := hashFile(filepath.Join(outDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[name] = h
	}
	return out, nil
}

func hashFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// hashContents hashes an in-memory file set, for comparing what the emitters
// would produce against what the manifest recorded.
func hashContents(files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for p, content := range files {
		sum := sha256.Sum256([]byte(content))
		out[p] = hex.EncodeToString(sum[:])
	}
	return out
}

// treeDelta is one disagreement between the manifest and the tree.
type treeDelta struct {
	Path string
	Kind string // "missing", "changed", "untracked"
}

func (d treeDelta) String() string { return d.Kind + " " + d.Path }

// diffTrees reports where got departs from want. Both are path -> hash.
//
// "missing" is recorded in the manifest and absent from disk; "untracked" is
// present on disk and unrecorded, which is what an entity removed from the
// schema leaves behind.
func diffTrees(want, got map[string]string) []treeDelta {
	var out []treeDelta
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			out = append(out, treeDelta{Path: p, Kind: "missing"})
		case g != w:
			out = append(out, treeDelta{Path: p, Kind: "changed"})
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			out = append(out, treeDelta{Path: p, Kind: "untracked"})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// manifestHashes reads the recorded file set back as a map.
func manifestHashes(m *generateManifest) map[string]string {
	out := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		out[f.Path] = f.SHA256
	}
	return out
}

// validateOutputDir returns output_dir as a clean relative path, or refuses it.
//
// generate removes the directories it owns before writing, so this value
// decides what gets deleted. It arrives from YAML. filepath.Join resolves ".."
// rather than neutralising it — the reason generatedFileName is guarded the
// same way — so "." would remove ./client and ./pb at the repository root, and
// "../x" or an absolute path would reach outside it.
func validateOutputDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("`output_dir` in tide.yaml is required")
	}
	if filepath.IsAbs(raw) {
		return "", fmt.Errorf("`output_dir` must be relative to the repository, got %q", raw)
	}
	clean := filepath.Clean(raw)
	if clean == "." || clean == string(filepath.Separator) {
		return "", fmt.Errorf("`output_dir` must name a directory inside the repository, got %q", raw)
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", fmt.Errorf("`output_dir` must stay inside the repository, got %q", raw)
		}
	}
	return clean, nil
}
