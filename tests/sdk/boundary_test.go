//go:build sdk

// SDK module-boundary test. Fails when the typed-client sub-module,
// github.com/rachitkumar205/atlantis/clients/go, imports anything from the main
// module.
//
// A caller repo sees the proto types, the gRPC stubs and the thin client
// wrappers, and none of internal/codegen, internal/dsl, internal/cache, the
// server implementation, the pgx pool or memcached.
//
// The `codegen` target runs `cd clients/go && go build ./...` (Makefile:375),
// which catches the same leak, since the SDK's go.mod does not require the main
// module and a leaked import fails to resolve. This test names the offending
// import path instead of producing a missing-module error indistinguishable
// from an unrelated one, and it runs with no replace directive in scope, which
// is how a consumer resolves the module.

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
	// The SDK module is nested inside the main module's path, so its own
	// packages carry mainModule as a prefix too and only sdkModule separates
	// them. Both end in `/` so a future `github.com/rachitkumar205/atlantis-foo`
	// does not match either.
	const (
		mainModule = "github.com/rachitkumar205/atlantis/"
		sdkModule  = "github.com/rachitkumar205/atlantis/clients/go/"
	)
	var leaks []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, mainModule) && !strings.HasPrefix(line, sdkModule) {
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
