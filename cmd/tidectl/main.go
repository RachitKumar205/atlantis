// tidectl — server-side admin CLI for atlantis.
//
// commands() below is the dispatch table and carries each subcommand's help
// line.
//
// codegen, plan, promote, lint and dev work on local files. migrate-up and
// migrate-down run golang-migrate against $PG_URL. adopt, inspect, history,
// blame, owners and rollback dial a running server through dialAdmin.
//
// The day-to-day caller workflow is `tide apply`; tidectl is for inspecting or
// repairing state.
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
	// hidden keeps a command reachable without listing it, for a renamed
	// command's old spelling: an existing pipeline keeps working and `tidectl`
	// with no arguments does not teach the retired name.
	hidden bool
}

// commands is the dispatch table.
//
// A function rather than a slice inside main(), so a test reads it as data. A
// test that string-matched this file would pin its formatting and go red on a
// reordering.
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
