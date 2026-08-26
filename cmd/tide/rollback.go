package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// cmdRollback — `tide rollback --to=<version> [--dry-run] [--yes]`
//
// Reverts the live schema to the state captured by a prior version.
// The server diffs current -> target, emits the migration SQL, and
// applies it in a single transaction.
//
// --dry-run prints the SQL that would run but does not apply.
// --yes skips the confirmation prompt.
func cmdRollback(args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	toVersion := fs.Int64("to", 0, "Target schema version to rollback to (required)")
	dryRun := fs.Bool("dry-run", false, "Print the SQL without executing")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if *toVersion <= 0 {
		fmt.Fprintln(os.Stderr, "tide rollback: --to=<version> is required")
		return 2
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}

	if *dryRun {
		// GetSchemaHistory finds the current version, then DiffSchemaVersions
		// reports the change without loading either one.
		return rollbackDryRun(cfg, *toVersion, *timeout)
	}

	if !*yes {
		fmt.Fprintf(os.Stderr, "Rolling back to schema version %d. This will modify the live database.\n", *toVersion)
		fmt.Fprint(os.Stderr, "Continue? [y/N] ")
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil || (answer != "y" && answer != "Y") {
			fmt.Fprintln(os.Stderr, "aborted")
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.RollbackSchema(ctx, &adminpb.RollbackSchemaRequest{
		ToVersion: *toVersion,
		Caller:    cfg.Caller,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}

	cliout.Successf("rolled back to version %d. New version: %s",
		*toVersion, cliout.Bold(fmt.Sprintf("%d", resp.GetNewVersion())))
	if sql := resp.GetUpSql(); sql != "" {
		fmt.Println()
		fmt.Println(cliout.Grey("Applied SQL:"))
		fmt.Println(sql)
	}
	return 0
}

func rollbackDryRun(cfg *tideConfig, toVersion int64, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	// Find current version from history.
	histResp, err := client.GetSchemaHistory(ctx, &adminpb.GetSchemaHistoryRequest{Limit: 1})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}
	if len(histResp.GetVersions()) == 0 {
		cliout.Errorf("no schema versions found")
		return 1
	}
	currentVersion := histResp.GetVersions()[0].GetVersion()

	diffResp, err := client.DiffSchemaVersions(ctx, &adminpb.DiffSchemaVersionsRequest{
		FromVersion: currentVersion,
		ToVersion:   toVersion,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide rollback:", err)
		return 3
	}

	fmt.Printf("%s v%d -> v%d\n\n",
		cliout.Bold("Rollback preview:"), currentVersion, toVersion)

	// Decoded into codegen.Diff, the type the server marshalled.
	//
	// This counted three named buckets and codegen.Diff has four. Rolling back
	// across a change that only dropped columns summed to zero and printed
	// "(no changes — schemas are identical)" — to an operator who was about to
	// run exactly that rollback. Of every place the missing bucket surfaced,
	// this one told the most dangerous lie, because the preview is the last
	// thing read before the rollback goes ahead.
	var d codegen.Diff
	if err := json.Unmarshal(diffResp.Diff, &d); err != nil {
		// Said out loud rather than skipped. The old code decoded under
		// `err == nil` and printed nothing at all on failure, so the preview
		// for a rollback it could not read looked identical to the preview for
		// a rollback with nothing in it — and the next line invites the
		// operator to run it for real.
		cliout.Warnf("could not decode the diff from the server: %v", err)
		fmt.Println(cliout.Grey("(this preview is empty because the diff did not parse, " +
			"not because the rollback is a no-op)"))
	} else {
		if d.IsEmpty() {
			fmt.Println(cliout.Grey("(no changes — schemas are identical)"))
			return 0
		}
		fmt.Printf("%d change(s) would be applied.\n", d.Len())

		// The tally, not just the total. A rollback that destroys rows and one
		// that adds a nullable column both read as "4 change(s)", and the
		// operator confirming this prompt is deciding between them.
		counts := map[codegen.ChangeClass]int{}
		for _, ch := range d.All() {
			counts[ch.Class]++
		}
		for _, s := range changeClassDisplay {
			if n := counts[s.class]; n > 0 {
				fmt.Printf("  %s\n", s.paint(fmt.Sprintf("%s%d %s", s.glyph, n, s.word)))
			}
		}
	}
	fmt.Println(cliout.Grey("(use without --dry-run to execute)"))
	return 0
}
