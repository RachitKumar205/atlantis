//go:build !cgo

package main

import (
	"fmt"
	"os"
)

// cmdSandbox is unavailable in a cgo-free build.
//
// `tide sandbox` is the only subcommand that compiles .atl files locally: it
// runs dsl.Parse and dsl.Lower in-process and executes SQL against the
// simulator, both of which reach Postgres's parser through pg_query_go — a cgo
// dependency with no pure-Go substitute. Every other subcommand is a request to
// the server, which does the parsing, so they all build without cgo.
//
// The prebuilt binaries are cgo-free so that `tide` cross-compiles to every
// platform and `go install` succeeds on a machine with no C toolchain. Users who
// want the local sandbox build from source, where cgo is on by default.
func cmdSandbox(args []string) int {
	fmt.Fprintln(os.Stderr, "tide: `sandbox` is not available in this build.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The sandbox compiles schema locally, which needs Postgres's parser (a cgo")
	fmt.Fprintln(os.Stderr, "dependency). Prebuilt binaries are cgo-free so they run everywhere without a")
	fmt.Fprintln(os.Stderr, "C toolchain. To use the sandbox, install a cgo build — this needs a C")
	fmt.Fprintln(os.Stderr, "compiler (on macOS: xcode-select --install):")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "    CGO_ENABLED=1 go install github.com/rachitkumar205/atlantis/cmd/tide@latest")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Every other tide subcommand works in this build.")
	return 3
}
