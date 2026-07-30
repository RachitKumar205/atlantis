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

// cmdHistory — `tide history [--limit N] [--caller X] [--format json]`
//
// Prints a paginated list of schema versions, newest first. Each row
// shows version number, caller, event type, change count, and timestamp.
func cmdHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	limit := fs.Int("limit", 25, "Max rows to return")
	caller := fs.String("caller", "", "Filter by caller name")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetSchemaHistory(ctx, &adminpb.GetSchemaHistoryRequest{
		Limit:  int32(*limit),
		Caller: *caller,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide history:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp, "diff", "ir_snapshot"); err != nil {
			fmt.Fprintln(os.Stderr, "tide history:", err)
			return 3
		}
	case "table":
		printHistoryTable(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide history: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printHistoryTable(resp *adminpb.GetSchemaHistoryResponse) {
	if len(resp.GetVersions()) == 0 {
		fmt.Println(cliout.Grey("  (no schema versions)"))
		return
	}

	fmt.Println()
	fmt.Printf("  %s\n", cliout.Bold("Schema History"))
	fmt.Println()

	for i, v := range resp.GetVersions() {
		isLast := i == len(resp.GetVersions())-1

		// Version dot + connector
		dot := cliout.Cyan("●")
		if v.GetEventType() == "seed" {
			dot = cliout.Grey("◌")
		} else if v.GetEventType() == "rollback" {
			dot = cliout.Yellow("●")
		}

		// Version label
		vLabel := cliout.Bold(cliout.Cyan(fmt.Sprintf("v%d", v.GetVersion())))

		// Event badge
		event := eventBadge(v.GetEventType())

		// Caller
		caller := cliout.Bold(v.GetCaller())

		// Timestamp
		ts := cliout.Grey(formatTimestamp(v.GetCreatedAt()))

		fmt.Printf("  %s %s  %s  %s  %s\n", dot, vLabel, event, caller, ts)

		// Changes detail line
		connector := cliout.Grey("│")
		if isLast {
			connector = " "
		}

		if v.GetChangeCount() > 0 {
			fmt.Printf("  %s      %s\n", connector, cliout.Green(fmt.Sprintf("+%d change(s)", v.GetChangeCount())))
		} else {
			fmt.Printf("  %s      %s\n", connector, cliout.Grey("no changes"))
		}

		if !isLast {
			fmt.Printf("  %s\n", cliout.Grey("│"))
		}
	}

	fmt.Println()
	if resp.GetHasMore() {
		fmt.Printf("  %s\n\n", cliout.Grey("… more versions available — use --limit"))
	}
}

func eventBadge(s string) string {
	switch s {
	case "apply":
		return cliout.Green("apply")
	case "rollback":
		return cliout.Yellow("rollback")
	case "adopt":
		return cliout.Cyan("adopt")
	case "seed":
		return cliout.Grey("seed")
	default:
		return s
	}
}

func formatTimestamp(raw string) string {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			ago := time.Since(t)
			switch {
			case ago < time.Minute:
				return "just now"
			case ago < time.Hour:
				return fmt.Sprintf("%dm ago", int(ago.Minutes()))
			case ago < 24*time.Hour:
				return fmt.Sprintf("%dh ago", int(ago.Hours()))
			default:
				return t.Format("Jan 02, 15:04")
			}
		}
	}
	if len(raw) > 16 {
		return raw[:16]
	}
	return raw
}
