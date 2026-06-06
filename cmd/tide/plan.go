package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

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

	var resp planResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/PlanSchema",
		planRequest{Caller: cfg.Caller, Files: files}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide plan:", err)
			return 3
		}
	case "table":
		printPlanReport(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide plan: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	if len(resp.ParseErrors) > 0 {
		return 3
	}
	switch resp.Class {
	case "additive":
		return 0
	case "backfill_required":
		return 1
	case "cross_caller_breaking":
		return 2
	default:
		fmt.Fprintf(os.Stderr, "tide plan: unknown plan class %q\n", resp.Class)
		return 3
	}
}

// printPlanReport renders the plan to stdout in a shape friendly to PR
// comments and human review. Order: parse errors first (any other field
// is meaningless if the schema didn't parse), then class, impact, and
// breaking detail.
func printPlanReport(resp planResponse) {
	if len(resp.ParseErrors) > 0 {
		cliout.Errorf("schema validation failed:")
		for _, e := range resp.ParseErrors {
			fmt.Printf("  %s %s\n", cliout.Coral(cliout.GlyphCross), e)
		}
		return
	}
	cliout.Header(os.Stdout, "plan")
	cliout.Field(os.Stdout, "plan_id", resp.PlanID)
	cliout.Field(os.Stdout, "class", colorClass(resp.Class))
	if len(resp.ImpactReport) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "impact")
		for _, e := range resp.ImpactReport {
			if e.Affected {
				cliout.Row(os.Stdout, "warn", cliout.Bold(e.Caller), e.Detail)
			} else {
				cliout.Row(os.Stdout, "muted", cliout.Faint(e.Caller), e.Detail)
			}
		}
	}
	if len(resp.BreakingDetail) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "breaking")
		for _, d := range resp.BreakingDetail {
			fmt.Printf("  %s  %s\n", cliout.Coral(cliout.GlyphCross), d)
		}
	}
	if len(resp.Extensions) > 0 {
		fmt.Println()
		printExtensions(resp.Extensions)
	}
}

// printExtensions renders the per-extension state the server reported
// in PlanResponse.Extensions. Three actions: ok (already enabled),
// enable (atlantis will CREATE EXTENSION inside the apply tx), missing
// (operator must install at OS level — apply will refuse).
func printExtensions(exts []extensionStatus) {
	cliout.Header(os.Stdout, "extensions")
	for _, e := range exts {
		switch e.Action {
		case "ok":
			cliout.Row(os.Stdout, "muted", e.Name, "already enabled")
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
			if e.InstallHint != "" {
				cliout.SubRow(os.Stdout, e.InstallHint)
			}
		}
	}
}

// colorClass paints the plan-class string by severity. Used in both
// `tide plan` and the apply impact report so the eye picks up the
// risk profile at a glance.
func colorClass(class string) string {
	switch class {
	case "additive":
		return cliout.Sage(class)
	case "backfill_required":
		return cliout.Brass(class)
	case "cross_caller_breaking":
		return cliout.Coral(cliout.Bold(class))
	case "unparseable":
		return cliout.Coral(class)
	}
	return class
}
