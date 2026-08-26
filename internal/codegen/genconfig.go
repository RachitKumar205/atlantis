package codegen

import "errors"

// InRepoModulePrefix is the client pb import root for generation inside this
// repository: cmd/tidectl writes the wrappers to clients/go/client, buf writes
// the matching pb to clients/go/pb, and go.mod carries the replace that
// resolves it.
const InRepoModulePrefix = "github.com/rachitkumar205/atlantis/clients/go"

// The server has defaults; the client has none.
//
// A generated CLIENT is caller code: it lives in the caller's module and
// imports pb from wherever that module put it, so no one value of ModulePrefix
// is right for a second caller. A generated SERVER is not caller code and
// cannot be — it imports `github.com/rachitkumar205/atlantis/internal/...`,
// and Go's internal rule makes those reachable only from inside this module.
// `tide generate` reflects that: it emits proto and client, never the server.
//
// So the server always compiles here, and its imports must resolve here.
// defaultServerPBPrefix is where `buf generate` actually writes pb in this
// repo (see buf.gen.yaml), and go.mod already carries the matching replace for
// the clients/go sub-module.
//
// A prefix that appears in neither go.mod nor go.sum yields code no module can
// build. This repo ships no .atl files, so the Makefile's `go build ./gen/...`
// compiles nothing and does not catch it; the compilecheck fixture below is
// what does.
const (
	defaultServerPBPrefix  = "github.com/rachitkumar205/atlantis/clients/go/pb"
	defaultServerPkgPrefix = "github.com/rachitkumar205/atlantis/gen/go/server"
)

// GenConfig parameterizes the Go import paths the emitters write.
type GenConfig struct {
	// ModulePrefix is the CLIENT's pb import root, and is required — the
	// client emitters reject an empty one. Caller-local generation sets it to
	// "<caller-module>/<output_dir>" so the generated wrappers import the
	// caller's own pb packages; generation inside this repo passes
	// InRepoModulePrefix.
	ModulePrefix string

	// ServerPBPrefix is the SERVER's pb import root, and ServerPkgPrefix is
	// the package path the emitted server itself occupies — register.go
	// imports the per-namespace packages by that path.
	//
	// Both exist so the compile fixture in internal/codegen/compilecheck can
	// be emitted into a location that is an ordinary package of this module.
	// Committing that output is what type-checks the emitter: `go build ./...`
	// compiles it, with no test harness to skip and no buf at test time.
	ServerPBPrefix  string
	ServerPkgPrefix string
}

// errNoModulePrefix is returned by the client emitters when ModulePrefix is
// empty. There is no prefix to fall back to: every value that resolves belongs
// to a module this one does not know the name of.
var errNoModulePrefix = errors.New(
	"GenConfig.ModulePrefix is required: it is the import root the generated " +
		"client imports pb from (codegen.InRepoModulePrefix for this repo)")

// pbImportPrefix is the path the generated client wrappers import their
// proto types from, minus the "/pb/atlantis/<ns>/v1" suffix. Empty unless
// the caller set it; the client emitters check before reaching here.
func (c GenConfig) pbImportPrefix() string {
	return c.ModulePrefix
}

// serverPBPrefix is the path the generated SERVER imports proto types from,
// minus the "/atlantis/<ns>/v1" suffix. Note the shape differs from the
// client's: buf writes this repo's pb under a "pb/" directory that is already
// part of the prefix.
func (c GenConfig) serverPBPrefix() string {
	if c.ServerPBPrefix == "" {
		return defaultServerPBPrefix
	}
	return c.ServerPBPrefix
}

// serverPkgPrefix is the package path the emitted server occupies.
func (c GenConfig) serverPkgPrefix() string {
	if c.ServerPkgPrefix == "" {
		return defaultServerPkgPrefix
	}
	return c.ServerPkgPrefix
}
