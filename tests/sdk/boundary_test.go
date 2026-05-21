//go:build sdk

// SDK module-boundary test. Verifies the typed-client sub-module
// (github.com/rachitkumar205/atlantis-go) never imports anything from atlantis's main
// module. The whole point of the sub-module split is that caller repos
// — backend, data-pipeline, future iOS / ML / web consumers — see only
// the proto types + gRPC stubs + thin client wrappers, never atlantis's
// internal/codegen, internal/dsl, internal/cache, the server impl, the
// pgx pool, memcached, or the DSL parser.
//
// The Makefile's `cd clients/go && go build ./...` step already enforces
// this at build time (the SDK's go.mod does not require github.com/rachitkumar205/atlantis,
// so any leaked import fails to resolve). This test is the explicit gate
// for CI — a fast, readable assertion that surfaces the violating import
// path rather than a compile error buried in proto-generated code.
//
// Why a separate test rather than relying on the build alone:
//   - `go build` failures from boundary breaches look identical to
//     unrelated missing-module errors. The test names the offender.
//   - CI gates need a single `go test` invocation to flip red; the
//     build step is upstream and harder to surface in a PR check.
//   - A future contributor might be tempted to add a replace directive
//     "just to unblock the build" — the test runs against the SDK as
//     a real consumer would see it, with no replaces in scope.

package sdk

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSDKHasNoAtlantisInternalImports asserts the SDK module's transitive
// dependency graph contains no package from `github.com/rachitkumar205/atlantis/*`. A
// failure here typically means a codegen template reached for an internal
// helper instead of duplicating the small amount of code into clients/go/.
func TestSDKHasNoAtlantisInternalImports(t *testing.T) {
	sdkDir := sdkModuleDir()
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = sdkDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list inside %s: %v\n%s", sdkDir, err, out)
	}
	var leaks []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// The SDK's own module path is `github.com/rachitkumar205/atlantis-go`; the leaked
		// path would be the main module `github.com/rachitkumar205/atlantis` (note the
		// missing `-go` suffix). Match the prefix exactly + a `/` to avoid
		// false-positives if someone names a future module `atlantis-foo`.
		if strings.HasPrefix(line, "github.com/rachitkumar205/atlantis/") {
			leaks = append(leaks, line)
		}
	}
	if len(leaks) > 0 {
		t.Errorf("SDK leaks %d atlantis-internal import(s):\n  - %s",
			len(leaks), strings.Join(leaks, "\n  - "))
	}
}

// sdkModuleDir resolves the absolute path of clients/go from this test
// file's compile-time location. Two dirs up is the atlantis repo root,
// then clients/go is the SDK sub-module.
func sdkModuleDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "clients", "go"))
}
