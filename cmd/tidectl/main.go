// tidectl — server-side admin CLI for atlantis.
//
// Subcommands:
//
//	tidectl codegen   Regenerate proto / Go / SQL / keys from a directory of .atl files.
//	tidectl plan      Diff a directory of .atl files against the IR checkpoint and
//	                emit the staged migration .up.sql / .down.sql.
//	tidectl promote   Move a staged migration into the migrations directory.
//	tidectl lint      Parse + lower every .atl file in a directory, exit 0 iff clean.
//	tidectl migrate-up   Run golang-migrate up against $PG_URL.
//	tidectl migrate-down Run golang-migrate down 1 against $PG_URL.
//	tidectl dev       One-shot local-dev loop: codegen + buf generate + go build +
//	                exec atlantis-server. Reads atlantis.dev.yaml (working-tree
//	                paths via source: local). Use for iteration; production
//	                deployments use the workspace.yaml + git refs path.
//	tidectl version   Print tidectl version.
//
// tidectl is *operator-shaped*. It runs against local files and (for migrate)
// the database. Day-to-day developer workflow is `tide apply`, not tidectl.
// tidectl is what the on-call operator reaches for when they need to
// inspect or repair state.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// version is set at build time via -ldflags "-X main.version=v0.X.Y".
// The default "dev" surfaces when run from a non-release build (go run,
// go install from source, IDE) so operators don't mistake an
// uninstalled binary for a release.
var version = "dev"

type command struct {
	name string
	help string
	fn   func(args []string) int
	// hidden keeps a command reachable without advertising it. Used for a
	// renamed command's old spelling: a pipeline that still calls it keeps
	// working, and nobody reading `tidectl` with no arguments learns the name
	// we are trying to retire.
	hidden bool
}

// commands is the dispatch table.
//
// A function rather than a slice built inside main() so a test can assert on
// it as data. The alternative was a test that string-matched this file's
// source, which pins the formatting rather than the property and goes red on a
// reordering that changes nothing.
func commands() []command {
	return []command{
		{"codegen", "Regenerate proto / Go / SQL / keys from .atl files", cmdCodegen, false},
		{"plan", "Stage a migration from the current .atl file set", cmdPlan, false},
		{"promote", "Promote a staged migration into migrations/", cmdPromote, false},
		{"lint", "Parse + lower every .atl; exit 0 iff clean", cmdLint, false},
		{"migrate-up", "Run golang-migrate up against $PG_URL", cmdMigrateUp, false},
		{"migrate-down", "Run golang-migrate down 1 against $PG_URL", cmdMigrateDown, false},
		{"dev", "Codegen + build + exec server from atlantis.dev.yaml (local iteration)", cmdDev, false},
		{"adopt", "Verify the live DB matches the declared .atl files and seed the IR checkpoint as the baseline", cmdAdopt, false},
		{"inspect", "Report how the live DB differs from the declared .atl files; writes nothing", cmdInspect, false},
		{"history", "Show schema version history", cmdHistory, false},
		{"blame", "Show per-field provenance for an entity", cmdBlame, false},
		{"owners", "Show entity ownership map", cmdOwners, false},
		{"rollback", "Revert to a prior schema version", cmdRollback, false},
		{"version", "Print tidectl version", cmdVersion, false},

		// Deprecated. `approve` now names an in-product decision made in the
		// console against a live database; this moves files in a repository.
		// See cmdPromote for why one word could not keep covering both.
		{"approve", "Deprecated alias for promote", cmdApproveAlias, true},
	}
}

func main() {
	cmds := commands()

	if len(os.Args) < 2 {
		printUsage(cmds)
		os.Exit(2)
	}

	sub := os.Args[1]
	for _, c := range cmds {
		if c.name == sub {
			os.Exit(c.fn(os.Args[2:]))
		}
	}
	fmt.Fprintf(os.Stderr, "tidectl: unknown subcommand %q\n\n", sub)
	printUsage(cmds)
	os.Exit(2)
}

func printUsage(cmds []command) { writeUsage(os.Stderr, cmds) }

func writeUsage(w io.Writer, cmds []command) {
	// Writes are discarded explicitly. errcheck exempts os.Stderr by default
	// and does not exempt an io.Writer, and usage output has nowhere useful to
	// report a failed write to anyway.
	_, _ = fmt.Fprintln(w, "usage: tidectl <subcommand> [args...]")
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "subcommands:")
	for _, c := range cmds {
		if c.hidden {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-14s %s\n", c.name, c.help)
	}
}

func cmdVersion(_ []string) int {
	cliout.Logo(os.Stdout, "tidectl", version)
	return 0
}

// flagSet is a small wrapper that always prefixes the subcommand into the
// usage message so `tidectl plan -h` reads naturally.
func flagSet(sub string) *flag.FlagSet {
	fs := flag.NewFlagSet("tidectl "+sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
