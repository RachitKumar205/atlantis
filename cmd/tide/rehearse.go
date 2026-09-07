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

// cmdRehearse executes the working tree's migration against a disposable
// clone of the managed database and reports what Postgres did. Exits with:
//
//	0 — pass, or pass with warnings
//	2 — fail_data or fail_structural: the real apply would fail the same way
//	3 — operational error (parse / network / config), the code every tide
//	    command reserves for it
//	4 — unverified: the rehearsal could not answer (size, timeout, quota),
//	    which an auto tier treats as "no"
//
// The server needs this caller rehearsal-enabled — a deliberate grant an
// admin makes from the console, because the clone holds every caller's rows.
func cmdRehearse(args []string) int {
	fs := flag.NewFlagSet("rehearse", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 20*time.Minute, "RPC timeout; a rehearsal clones the database first")
	format := fs.String("format", "table", "Output format: table or json")
	if err := cliflag.ParseNoArgs(fs, args); err != nil {
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
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.RehearseMigration(ctx, &adminpb.RehearseMigrationRequest{
		Caller: cfg.Caller, Files: files,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide rehearse:", explainAuthz(err))
		return 3
	}

	if *format == "json" {
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide rehearse:", err)
			return 3
		}
		return rehearseExitCode(resp.GetVerdict())
	}

	printRehearsalReport(resp)
	return rehearseExitCode(resp.GetVerdict())
}

func rehearseExitCode(verdict string) int {
	switch verdict {
	case "pass", "pass_with_warnings":
		return 0
	case "fail_data", "fail_structural":
		return 2
	}
	return 4
}

func printRehearsalReport(resp *adminpb.RehearseMigrationResponse) {
	cliout.Header(os.Stdout, "rehearsal")
	tone := "brass"
	switch resp.GetVerdict() {
	case "pass":
		tone = "ok"
	case "fail_data", "fail_structural":
		tone = "bad"
	}
	cliout.Row(os.Stdout, tone, "verdict", resp.GetVerdict())
	if resp.GetReason() != "" {
		cliout.Row(os.Stdout, "muted", "reason", resp.GetReason())
	}
	if resp.GetError() != "" {
		cliout.Row(os.Stdout, "muted", "error", resp.GetError())
	}
	for constraint, n := range resp.GetDiagnostics() {
		cliout.Row(os.Stdout, "muted", "violations", fmt.Sprintf("%s: %d", constraint, n))
	}
	if resp.GetRemediation() != "" {
		cliout.Row(os.Stdout, "brass", "suggested", resp.GetRemediation())
	}
	cliout.Row(os.Stdout, "muted", "timing",
		fmt.Sprintf("clone %dms, execute %dms", resp.GetCloneMs(), resp.GetExecuteMs()))
	fmt.Println()
	switch resp.GetVerdict() {
	case "pass", "pass_with_warnings":
		fmt.Println("The migration ran cleanly against a clone of the real data.")
	case "fail_data", "fail_structural":
		fmt.Println("The real apply would fail the same way. Fix the schema or the data, then rehearse again.")
	default:
		fmt.Println("The rehearsal could not answer; an auto tier treats this as unverified.")
	}
	return
}
