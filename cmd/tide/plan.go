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
func cmdPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	against := fs.String("against", "", "Server endpoint override (host:port); defaults to tide.yaml's endpoint")
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
	if *against != "" {
		cfg.Endpoint = *against
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

// exitCodeForClass maps a plan class to tide's exit code.
//
// This is the contract CI branches on — docs/guides/set-up-caller-ci.md tells
// readers to gate merges on it — so the mapping is stated once and tested,
// rather than written inline where a mechanical edit could renumber a case
// without anything noticing.
//
// An unparseable plan shares the default arm with a class this binary does not
// recognize. Both mean "do not proceed", and both are operational failures
// rather than plan outcomes: exiting 0 or 2 on a class whose risk this tide
// cannot assess would be guessing.
func exitCodeForClass(c adminpb.PlanClass) int {
	switch c {
	case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
		return 0
	case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
		return 1
	case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
		return 2
	default:
		return 3
	}
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

// colorClass paints the plan-class string by severity. Used in both
// `tide plan` and the apply impact report so the eye picks up the
// risk profile at a glance.
func colorClass(class adminpb.PlanClass) string {
	name := planClassName(class)
	switch class {
	case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
		return cliout.Sage(name)
	case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
		return cliout.Brass(name)
	case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
		return cliout.Coral(cliout.Bold(name))
	case adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:
		return cliout.Coral(name)
	}
	return name
}
