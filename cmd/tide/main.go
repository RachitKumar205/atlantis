// tide — caller-side CLI invoked from inside caller repos.
//
// printUsage below lists every subcommand; TestEverySubcommandAppearsInUsage
// holds it level with the dispatch switch.
//
// tide reads tide.yaml from the working directory for the caller name and the
// schema paths, submits the .atl files to AdminService.PlanSchema, and routes
// the plan class:
//
//   - additive               → ApplyMigration, regenerate the local client
//   - backfill_required      → print the expected backfill, exit 1
//   - cross_caller_breaking  → print the impact report, exit 2
//   - destructive            → print what is destroyed, exit 4
//   - unparseable            → print the validation errors, exit 3
//
// Destructive exits 4 rather than joining breaking on 2 so CI can gate on the
// two separately: a breaking change is cleared by shipping the other callers'
// updates, a destructive one by deciding whether losing the rows is intended.
//
// `tide apply` runs `tide pull` first, so cross-caller references resolve
// against the current merged schema. --no-pull suppresses it.
//
// Transport is protobuf over gRPC through the generated admin stubs in
// clients/go. The mTLS material and the endpoint come from the credential store
// `tide login` writes, and neither is settable in tide.yaml.
package main

import (
	"fmt"
	"os"

	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// version is set at build time via -ldflags "-X main.version=v0.X.Y".
// The default "dev" surfaces when run from a non-release build (go run,
// go install from source, IDE) so operators don't mistake an
// uninstalled binary for a release.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		os.Exit(cmdInit(os.Args[2:]))
	case "apply":
		os.Exit(cmdApply(os.Args[2:]))
	case "plan":
		os.Exit(cmdPlan(os.Args[2:]))
	case "rehearse":
		os.Exit(cmdRehearse(os.Args[2:]))
	case "inspect":
		os.Exit(cmdInspect(os.Args[2:]))
	case "pull":
		os.Exit(cmdPull(os.Args[2:]))
	case "generate":
		os.Exit(cmdGenerate(os.Args[2:]))
	case "list":
		os.Exit(cmdList(os.Args[2:]))
	case "show":
		os.Exit(cmdShow(os.Args[2:]))
	case "backfill":
		os.Exit(cmdBackfill(os.Args[2:]))
	case "job":
		os.Exit(cmdJob(os.Args[2:]))
	case "workflow":
		os.Exit(cmdWorkflow(os.Args[2:]))
	case "history":
		os.Exit(cmdHistory(os.Args[2:]))
	case "diff":
		os.Exit(cmdDiff(os.Args[2:]))
	case "blame":
		os.Exit(cmdBlame(os.Args[2:]))
	case "owners":
		os.Exit(cmdOwners(os.Args[2:]))
	case "parked":
		os.Exit(cmdParked(os.Args[2:]))
	case "rollback":
		os.Exit(cmdRollback(os.Args[2:]))
	case "sandbox":
		os.Exit(cmdSandbox(os.Args[2:]))
	case "caller":
		os.Exit(cmdCaller(os.Args[2:]))
	case "login":
		os.Exit(cmdLogin(os.Args[2:]))
	case "version":
		cliout.Logo(os.Stdout, "tide", version)
	default:
		fmt.Fprintf(os.Stderr, "tide: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	cliout.LogoInline(os.Stderr, "tide", version)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "usage: tide init     --caller NAME [--org ORG] [--output-dir DIR]")
	fmt.Fprintln(os.Stderr, "       tide apply    [--backfill] [--dry-run] [--no-pull]")
	fmt.Fprintln(os.Stderr, "       tide plan     [--format table|json] [--no-pull]")
	fmt.Fprintln(os.Stderr, "       tide rehearse [--format table|json] [--timeout D]")
	fmt.Fprintln(os.Stderr, "       tide inspect  [--format table|json]")
	fmt.Fprintln(os.Stderr, "       tide pull     [--force]")
	fmt.Fprintln(os.Stderr, "       tide generate [--check [--against-server]]")
	fmt.Fprintln(os.Stderr, "       tide list")
	fmt.Fprintln(os.Stderr, "       tide show     <path-substring>")
	fmt.Fprintln(os.Stderr, "       tide backfill status [plan-hash]")
	fmt.Fprintln(os.Stderr, "       tide job      submit|status|dead|retry ...")
	fmt.Fprintln(os.Stderr, "       tide workflow start|status ...")
	fmt.Fprintln(os.Stderr, "       tide history  [--limit N] [--caller X]")
	fmt.Fprintln(os.Stderr, "       tide diff     <from-version> <to-version>")
	fmt.Fprintln(os.Stderr, "       tide blame    <entity-id>")
	fmt.Fprintln(os.Stderr, "       tide owners")
	fmt.Fprintln(os.Stderr, "       tide parked   [--all] [--format table|json]")
	fmt.Fprintln(os.Stderr, "       tide rollback --to=<version> [--dry-run] [--yes]")
	fmt.Fprintln(os.Stderr, "       tide sandbox  boot|shell|spawn ...")
	fmt.Fprintln(os.Stderr, "       tide caller   alias list|add|rm <caller> [alias]")
	fmt.Fprintln(os.Stderr, "       tide login    [--caller NAME] [--oidc [--audience AUD]] (or --url URL --org ORG --token TOKEN)")
	fmt.Fprintln(os.Stderr, "       tide version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Reads ./tide.yaml for the caller name and schema paths.")
	fmt.Fprintln(os.Stderr, "Credentials and the atlantis address come from `tide login`.")
}
