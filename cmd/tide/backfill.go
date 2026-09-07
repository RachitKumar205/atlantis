package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliflag"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdBackfill is the top-level `tide backfill` dispatcher.
//
//	tide backfill status              latest plan for this caller
//	tide backfill status <plan-hash>  specific plan
func cmdBackfill(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tide backfill status [plan-hash]")
		return 2
	}
	switch args[0] {
	case "status":
		return cmdBackfillStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide backfill: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: tide backfill status [plan-hash]")
		return 2
	}
}

// cmdBackfillStatus calls GetBackfillStatus and pretty-prints. Exit codes:
//
//	0 — status retrieved (regardless of whether the plan is complete or failed)
//	1 — plan is in a failed state
//	3 — operational error (network, config, missing plan)
func cmdBackfillStatus(args []string) int {
	fs := flag.NewFlagSet("backfill status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	format := fs.String("format", "table", "Output format: table or json")
	pos, err := cliflag.Parse(fs, args)
	if err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	req := &adminpb.GetBackfillStatusRequest{}
	if len(pos) == 0 {
		req.LatestForCaller = cfg.Caller
	} else {
		req.PlanHash = pos[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetBackfillStatus(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide backfill status:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide backfill status:", err)
			return 3
		}
	case "table":
		printBackfillStatus(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide backfill status: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	if resp.GetStatus() == "failed" {
		return 1
	}
	return 0
}

func printBackfillStatus(s *adminpb.GetBackfillStatusResponse) {
	fmt.Printf("%s %s\n", cliout.Grey("plan-hash:"), cliout.Bold(s.GetPlanHash()))
	fmt.Printf("%s    %s\n", cliout.Grey("caller:"), s.GetCaller())
	fmt.Printf("%s    %s\n", cliout.Grey("status:"), colorBackfillStatus(s.GetStatus()))
	fmt.Printf("%s   %s\n", cliout.Grey("started:"), s.GetStartedAt())
	if s.GetCompletedAt() != "" {
		fmt.Printf("%s %s\n", cliout.Grey("completed:"), s.GetCompletedAt())
	}
	if s.GetErrorMsg() != "" {
		fmt.Printf("%s     %s\n", cliout.Red("error:"), s.GetErrorMsg())
	}
	fmt.Println()

	if len(s.GetFields()) == 0 {
		fmt.Println(cliout.Grey("(no fields declared)"))
		return
	}
	fmt.Println(cliout.Bold("fields:"))
	for _, f := range s.GetFields() {
		name := fmt.Sprintf("%s.%s", f.GetEntityId(), cliout.Cyan(f.GetField()))
		extra := ""
		if pk := f.GetLastPk(); pk != "" && pk != "0" {
			extra = cliout.Grey(fmt.Sprintf("   last_pk=%s", pk))
		}
		errInfo := ""
		if f.GetErrorMsg() != "" {
			errInfo = "   " + cliout.Red("err="+f.GetErrorMsg())
		}
		fmt.Printf("  %-50s  %-20s  rows=%d%s%s\n", name, colorBackfillStatus(f.GetStatus()), f.GetRowsProcessed(), extra, errInfo)
	}
	if s.GetStatus() == "phase2_running" {
		fmt.Println()
		fmt.Println(cliout.Grey("phase 3 (SET NOT NULL + DROP INDEX) runs automatically when every field is complete."))
	}
}

func colorBackfillStatus(s string) string {
	switch s {
	case "complete":
		return cliout.Green(s)
	case "running", "phase2_running", "phase3_running":
		return cliout.Yellow(s)
	case "failed":
		return cliout.Red(cliout.Bold(s))
	case "pending":
		return cliout.Grey(s)
	}
	return s
}
