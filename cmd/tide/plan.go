package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdPlan exits with:
//
//	0 — additive
//	1 — backfill required
//	2 — breaking
//	3 — operational error (parse / network / config)
//
// Same code map as cmdApply so the two commands compose in CI workflows
// without per-step translation. The plan RPC is read-only on the server
// side, so a `tide plan` is safe to run from any pre-merge environment
// (including against a production endpoint).
//
// --against is gone. It let a caller point at any server, which is the thing
// this effort removed: the organisation owns the address and `tide login`
// collects it. Aiming a dry run elsewhere means enrolling there.
func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	noPull := fs.Bool("no-pull", false, "Skip the pre-plan refresh of .tide-cache/")
	format := fs.String("format", "table", "Output format: table or json")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	files, err := collectPCFiles(cfg.SchemaPaths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "tide: no .atl files found under %v\n", cfg.SchemaPaths)
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Refresh the local cache so cross-caller references resolve against
	// the freshest merged view. Non-fatal on failure: the server is the
	// definitive view anyway, and a stale cache only affects local IDE
	// hints.
	if !*noPull {
		pullBeforeApply(ctx, cfg)
	}

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.PlanSchema(ctx, &adminpb.PlanSchemaRequest{
		Caller: cfg.Caller, Files: files,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide plan:", err)
			return 3
		}
	case "table":
		printPlanReport(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide plan: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	if len(resp.GetParseErrors()) > 0 {
		return 3
	}
	code := exitCodeForClass(resp.GetClass())
	if code == 3 {
		fmt.Fprintf(os.Stderr, "tide plan: cannot act on plan class %q\n", resp.GetClass())
	}
	return code
}

// classExitCode is the contract CI branches on.
//
// docs/guides/set-up-caller-ci.md tells readers to gate merges on these, so a
// renumbering here changes whether other people's pull requests can merge. It
// is a map rather than a switch so that completeness over the proto enum is
// something a test can iterate; a switch's default arm answers for a class
// nobody decided about, and answers plausibly, which is how destructive spent a
// release reported as an operational failure.
//
// Codes 0-2 are plan outcomes. 3 is reserved for "this tide cannot act on it"
// and is deliberately NOT in this map — see exitCodeForClass.
var classExitCode = map[adminpb.PlanClass]int{
	adminpb.PlanClass_PLAN_CLASS_ADDITIVE:              0,
	adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:     1,
	adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING: 2,

	// 4, not 2. Code 2 already means both "unknown subcommand" and "breaks
	// another caller"; folding a third meaning in would leave CI unable to tell
	// a change that destroys rows from one that merely needs another team's
	// sign-off. Those call for different responses, and the change policy this
	// is heading toward is per-class, so the exit code has to be too.
	//
	// 4 is additive to the contract: nothing that checks `-eq 2` today changes
	// meaning.
	adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE: 4,
}

// exitCodeForClass maps a plan class to tide's exit code.
//
// Unparseable and a class this binary does not recognize share exit 3 by
// falling out of the map. Both mean "do not proceed", and both are operational
// failures rather than plan outcomes: exiting 0 or 2 on a class whose risk this
// tide cannot assess would be guessing.
//
// TestEveryDeclaredClassIsHandled asserts every class declared in admin.proto
// has an entry here or an explicit decision to leave it out.
func exitCodeForClass(c adminpb.PlanClass) int {
	if code, ok := classExitCode[c]; ok {
		return code
	}
	return 3
}

// printPlanReport renders the plan to stdout in a shape friendly to PR
// comments and human review. Order: parse errors first (any other field
// is meaningless if the schema didn't parse), then class, impact, and
// breaking detail.
func printPlanReport(resp *adminpb.PlanSchemaResponse) {
	if len(resp.GetParseErrors()) > 0 {
		cliout.Errorf("schema validation failed:")
		for _, e := range resp.GetParseErrors() {
			fmt.Printf("  %s %s\n", cliout.Coral(cliout.GlyphCross), e)
		}
		return
	}
	cliout.Header(os.Stdout, "plan")
	cliout.Field(os.Stdout, "plan_id", resp.GetPlanId())
	cliout.Field(os.Stdout, "class", colorClass(resp.GetClass()))
	// The server decides this and says so; tide does not derive it from the
	// class. The rule is a row in a table only the server reads, so a CLI that
	// worked it out from the class would print one answer while the apply
	// enforced another.
	if resp.GetRequiresApproval() {
		cliout.Field(os.Stdout, "approval", cliout.Brass(
			fmt.Sprintf("required (%s)", resp.GetApproverRole())))
	}
	if len(resp.GetImpactReport()) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "impact")
		for _, e := range resp.GetImpactReport() {
			if e.GetAffected() {
				cliout.Row(os.Stdout, "warn", cliout.Bold(e.GetCaller()), e.GetDetail())
			} else {
				cliout.Row(os.Stdout, "muted", cliout.Faint(e.GetCaller()), e.GetDetail())
			}
		}
	}
	if len(resp.GetBreakingDetail()) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "breaking")
		for _, d := range resp.GetBreakingDetail() {
			fmt.Printf("  %s  %s\n", cliout.Coral(cliout.GlyphCross), d)
		}
	}
	if len(resp.GetExtensions()) > 0 {
		fmt.Println()
		printExtensions(resp.GetExtensions())
	}
}

// printExtensions renders the per-extension state the server reported
// in PlanResponse.Extensions. Three actions: ok (already enabled),
// enable (atlantis will CREATE EXTENSION inside the apply tx), missing
// (operator must install at OS level — apply will refuse).
func printExtensions(exts []*adminpb.ExtensionStatus) {
	cliout.Header(os.Stdout, "extensions")
	for _, e := range exts {
		switch e.GetAction() {
		case "ok":
			cliout.Row(os.Stdout, "muted", e.GetName(), "already enabled")
		case "enable":
			cliout.Row(os.Stdout, "brass", e.Name, "will be auto-enabled")
			if e.Trigger != "" {
				cliout.SubRow(os.Stdout, e.Trigger)
			}
		case "missing":
			cliout.Row(os.Stdout, "coral", e.Name, "missing")
			if e.Trigger != "" {
				cliout.SubRow(os.Stdout, e.Trigger)
			}
			if e.GetInstallHint() != "" {
				cliout.SubRow(os.Stdout, e.GetInstallHint())
			}
		}
	}
}

// classPaint colours a plan class by severity.
//
// A map for the same reason classExitCode is one: a class with no entry here
// used to print in the terminal's default colour, which reads as "nothing
// notable" — the opposite of what a destructive plan is. Completeness is
// asserted by TestEveryDeclaredClassHasAColour.
//
// Destructive and breaking share Coral+Bold, and that is a real answer rather
// than an oversight. The palette has one alarm colour, both classes are at the
// top of the severity scale, and the two are told apart by their names, which
// colorClass paints rather than replaces.
var classPaint = map[adminpb.PlanClass]func(string) string{
	adminpb.PlanClass_PLAN_CLASS_ADDITIVE:          cliout.Sage,
	adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED: cliout.Brass,
	adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING: func(s string) string {
		return cliout.Coral(cliout.Bold(s))
	},
	adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE: func(s string) string {
		return cliout.Coral(cliout.Bold(s))
	},
	adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE: cliout.Coral,
}

// colorClass paints the plan-class string by severity. Used in both
// `tide plan` and the apply impact report so the eye picks up the
// risk profile at a glance.
func colorClass(class adminpb.PlanClass) string {
	name := planClassName(class)
	if paint, ok := classPaint[class]; ok {
		return paint(name)
	}
	return name
}
