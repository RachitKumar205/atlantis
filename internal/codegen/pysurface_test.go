// Holds the emitted Python trees level with the runtime they import.
//
// Every emitted client, custom-query and jobs file opens with `from
// atlantis_client import ...`, so a rename in clients/python breaks the
// caller's build, not this repository's. A Python test cannot see a change to
// pyclient.go and a Go test cannot import the package, so the contract is
// asserted from both sides.
package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// pyRuntimeRoot is the runtime package the emitted trees import.
const pyRuntimeRoot = "../../clients/python/src/atlantis_client"

// pyGoldenRoot is the committed output of a full generation.
const pyGoldenRoot = "testdata/golden"

// pyContractFile holds the same surface as a table, and is the copy that runs
// against an installed wheel.
const pyContractFile = "../../clients/python/tests/test_public_surface.py"

// minSurfaceModules and minSurfaceNames are the floors a broken scan falls
// through.
//
// Without them a regexp that stops matching leaves every assertion iterating an
// empty set and passing.
const (
	minSurfaceModules = 4
	minSurfaceNames   = 6
)

// A module import binds an alias; the names it carries are the attributes read
// off that alias. A name import carries the names itself.
var (
	pyModuleImport = regexp.MustCompile(`from atlantis_client import ([a-z_]+)(?: as ([A-Za-z_][A-Za-z0-9_]*))?`)
	pyNameImport   = regexp.MustCompile(`from atlantis_client\.([a-z_][a-z0-9_.]*) import ([A-Za-z_][A-Za-z0-9_, ]*)`)
)

// TestGoldenTreeImportsResolve reads the committed generation and requires
// every module and name it imports to exist in the runtime.
func TestGoldenTreeImportsResolve(t *testing.T) {
	surface := pySurfaceOf(t, pySources(t, pyGoldenRoot, ".py"))
	assertPySurfaceResolves(t, surface)
}

// TestEmitterImportsResolve reads the emitters themselves.
//
// pyclient.go writes the tenant import only for a partitioned entity, so an
// import added for a shape the golden tree does not exercise is invisible to
// the scan above.
func TestEmitterImportsResolve(t *testing.T) {
	surface := pySurfaceOf(t, pySources(t, ".", ".go"))
	assertPySurfaceResolves(t, surface)
}

// assertPySurfaceResolves maps each module to a file under the runtime and
// requires the file to bind each name.
func assertPySurfaceResolves(t *testing.T, surface map[string]map[string]string) {
	t.Helper()

	names := 0
	for _, byName := range surface {
		names += len(byName)
	}
	if len(surface) < minSurfaceModules || names < minSurfaceNames {
		t.Fatalf("scanned %d modules and %d names, want at least %d and %d — the scan is not reading what it thinks it is",
			len(surface), names, minSurfaceModules, minSurfaceNames)
	}

	for _, module := range sortedPyKeys(surface) {
		path, err := resolvePyModule(module)
		if err != nil {
			t.Errorf("%s: %v", module, err)
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, name := range sortedPyKeys(surface[module]) {
			if !bindsPyName(string(source), name) {
				t.Errorf("%s imports %s.%s, which %s does not bind",
					surface[module][name], module, name, path)
			}
		}
	}
}

// pySources reads every file under root with the given suffix, skipping Go
// test files so this file's own patterns are not scanned as imports.
func pySources(t *testing.T, root, suffix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, suffix) || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// pySurfaceOf maps each imported module to the names read off it, and each
// name to the file that wanted it.
func pySurfaceOf(t *testing.T, sources map[string]string) map[string]map[string]string {
	t.Helper()
	surface := map[string]map[string]string{}

	add := func(module, name, where string) {
		if surface[module] == nil {
			surface[module] = map[string]string{}
		}
		if name != "" {
			surface[module][name] = where
		}
	}

	for path, source := range sources {
		for _, m := range pyModuleImport.FindAllStringSubmatch(source, -1) {
			module, alias := "atlantis_client."+m[1], m[1]
			if m[2] != "" {
				alias = m[2]
			}
			add(module, "", path)
			uses := regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.([A-Za-z_][A-Za-z0-9_]*)`)
			for _, u := range uses.FindAllStringSubmatch(source, -1) {
				add(module, u[1], path)
			}
		}
		for _, m := range pyNameImport.FindAllStringSubmatch(source, -1) {
			module := "atlantis_client." + m[1]
			for _, name := range strings.Split(m[2], ",") {
				add(module, strings.TrimSpace(name), path)
			}
		}
	}
	return surface
}

// resolvePyModule returns the file a dotted module name is stored in.
func resolvePyModule(module string) (string, error) {
	rel := strings.TrimPrefix(module, "atlantis_client.")
	base := filepath.Join(pyRuntimeRoot, filepath.FromSlash(strings.ReplaceAll(rel, ".", "/")))
	for _, candidate := range []string{base + ".py", filepath.Join(base, "__init__.py")} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no module file under %s", pyRuntimeRoot)
}

// bindsPyName reports whether a module defines a name or re-exports it.
func bindsPyName(source, name string) bool {
	n := regexp.QuoteMeta(name)
	binding := regexp.MustCompile(`(?m)^(?:async def|def|class)\s+` + n + `\b` +
		`|^` + n + `\s*[:=]` +
		`|^from\s+\S+\s+import\s+[^\n]*\b` + n + `\b`)
	return binding.MatchString(source)
}

func sortedPyKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CONTRACT is a table of module to names; the entries are one per line.
var (
	pyContractBlock = regexp.MustCompile(`(?s)CONTRACT[^=]*=\s*\{(.*?)\n\}`)
	pyContractEntry = regexp.MustCompile(`"([a-z_][a-z0-9_.]*)":\s*\(([^)]*)\)`)
)

// TestPythonContractCoversTheEmitters compares the table in
// clients/python/tests/test_public_surface.py with what the emitters and the
// golden tree import.
//
// That table is the copy that runs against the installed wheel, where no
// emitter is readable. An import added to an emitter extends the scans above
// and leaves the table behind, so the wheel check stops covering it.
func TestPythonContractCoversTheEmitters(t *testing.T) {
	scanned := pySurfaceOf(t, pySources(t, pyGoldenRoot, ".py"))
	for module, names := range pySurfaceOf(t, pySources(t, ".", ".go")) {
		if scanned[module] == nil {
			scanned[module] = map[string]string{}
		}
		for name, where := range names {
			scanned[module][name] = where
		}
	}

	contract := pyContract(t)
	if len(contract) < minSurfaceModules {
		t.Fatalf("read %d modules from %s, want at least %d",
			len(contract), pyContractFile, minSurfaceModules)
	}

	for _, module := range sortedPyKeys(scanned) {
		want, ok := contract[module]
		if !ok {
			t.Errorf("%s imports %s, which %s does not list",
				anyPySource(scanned[module]), module, pyContractFile)
			continue
		}
		for _, name := range sortedPyKeys(scanned[module]) {
			if !want[name] {
				t.Errorf("%s imports %s.%s, which %s does not list",
					scanned[module][name], module, name, pyContractFile)
			}
		}
	}

	for _, module := range sortedPyKeys(contract) {
		if scanned[module] == nil {
			t.Errorf("%s lists %s, which no emitter imports", pyContractFile, module)
			continue
		}
		for _, name := range sortedPyKeys(contract[module]) {
			if _, ok := scanned[module][name]; !ok {
				t.Errorf("%s lists %s.%s, which no emitter imports", pyContractFile, module, name)
			}
		}
	}
}

// pyContract reads the CONTRACT table.
func pyContract(t *testing.T) map[string]map[string]bool {
	t.Helper()
	source, err := os.ReadFile(pyContractFile)
	if err != nil {
		t.Fatalf("read %s: %v", pyContractFile, err)
	}
	block := pyContractBlock.FindStringSubmatch(string(source))
	if block == nil {
		t.Fatalf("%s has no CONTRACT table", pyContractFile)
	}

	out := map[string]map[string]bool{}
	for _, entry := range pyContractEntry.FindAllStringSubmatch(block[1], -1) {
		names := map[string]bool{}
		for _, name := range strings.Split(entry[2], ",") {
			name = strings.Trim(strings.TrimSpace(name), `"`)
			if name != "" {
				names[name] = true
			}
		}
		out[entry[1]] = names
	}
	return out
}

// anyPySource names one file that wanted a module.
func anyPySource(names map[string]string) string {
	for _, name := range sortedPyKeys(names) {
		return names[name]
	}
	return "an emitter"
}
