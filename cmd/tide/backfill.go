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

type backfillStatusRequest struct {
	PlanHash        string `json:"PlanHash,omitempty"`
	LatestForCaller string `json:"LatestForCaller,omitempty"`
}

type backfillFieldStatus struct {
	EntityID      string `json:"EntityID"`
	Field         string `json:"Field"`
	Status        string `json:"Status"`
	RowsProcessed int64  `json:"RowsProcessed"`
	LastPK        string `json:"LastPK"`
	ErrorMsg      string `json:"ErrorMsg"`
}

type backfillStatusResponse struct {
	PlanHash    string                `json:"PlanHash"`
	Caller      string                `json:"Caller"`
	Status      string                `json:"Status"`
	ErrorMsg    string                `json:"ErrorMsg"`
	StartedAt   string                `json:"StartedAt"`
	CompletedAt string                `json:"CompletedAt"`
	Fields      []backfillFieldStatus `json:"Fields"`
}

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
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	req := backfillStatusRequest{}
	if fs.NArg() == 0 {
		req.LatestForCaller = cfg.Caller
	} else {
		req.PlanHash = fs.Arg(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	var resp backfillStatusResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/GetBackfillStatus", req, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tide backfill status:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide backfill status:", err)
			return 3
		}
	case "table":
		printBackfillStatus(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide backfill status: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	if resp.Status == "failed" {
		return 1
	}
	return 0
}

func printBackfillStatus(s backfillStatusResponse) {
	fmt.Printf("%s %s\n", cliout.Grey("plan-hash:"), cliout.Bold(s.PlanHash))
	fmt.Printf("%s    %s\n", cliout.Grey("caller:"), s.Caller)
	fmt.Printf("%s    %s\n", cliout.Grey("status:"), colorBackfillStatus(s.Status))
	fmt.Printf("%s   %s\n", cliout.Grey("started:"), s.StartedAt)
	if s.CompletedAt != "" {
		fmt.Printf("%s %s\n", cliout.Grey("completed:"), s.CompletedAt)
	}
	if s.ErrorMsg != "" {
		fmt.Printf("%s     %s\n", cliout.Red("error:"), s.ErrorMsg)
	}
	fmt.Println()

	if len(s.Fields) == 0 {
		fmt.Println(cliout.Grey("(no fields declared)"))
		return
	}
	fmt.Println(cliout.Bold("fields:"))
	for _, f := range s.Fields {
		name := fmt.Sprintf("%s.%s", f.EntityID, cliout.Cyan(f.Field))
		extra := ""
		if f.LastPK != "" && f.LastPK != "0" {
			extra = cliout.Grey(fmt.Sprintf("   last_pk=%s", f.LastPK))
		}
		errInfo := ""
		if f.ErrorMsg != "" {
			errInfo = "   " + cliout.Red("err="+f.ErrorMsg)
		}
		fmt.Printf("  %-50s  %-20s  rows=%d%s%s\n", name, colorBackfillStatus(f.Status), f.RowsProcessed, extra, errInfo)
	}
	if s.Status == "phase2_running" {
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
