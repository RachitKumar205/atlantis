package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdInspect exits with:
//
//	0 — the database matches the declaration
//	1 — outstanding work: something is declared that the database lacks,
//	    or holds something the declaration no longer mentions
//	2 — mismatch: both sides exist and disagree
//	3 — operational error (parse / network / config)
//
// 3 stays "operational" as it does for plan and apply, so a CI step can tell
// "your schema and your database differ" from "the command could not run"
// without parsing output. 1 and 2 are separated because they call for
// different actions: 1 is usually a `tide apply` away, 2 means somebody
// changed the database out from under the declaration and no apply will
// reconcile it.
//
// # What this replaces
//
// `tidectl adopt --allow-drift` was how an operator found out what differed.
// That is a flag whose name promises a write, on a command that rewrites the
// shared checkpoint for every caller — so answering a question required a
// privilege that could change everything, and a manifest, and a binary
// callers do not have. This asks the question on its own, at
// CAPABILITY_SCHEMA_READ, from the caller's own repo.
func cmdInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	against := fs.String("against", "", "Server endpoint override (host:port); defaults to tide.yaml's endpoint")
	timeout := fs.Duration("timeout", 120*time.Second, "RPC timeout (introspecting a large schema takes a while)")
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

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.InspectSchema(ctx, &adminpb.InspectSchemaRequest{
		Caller: cfg.Caller, Files: files,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide inspect:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide inspect:", err)
			return 3
		}
	case "table":
		printInspectReport(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide inspect: unknown --format %q (want table|json)\n", *format)
		return 3
	}

	switch {
	case resp.GetMismatchCount() > 0:
		return 2
	case !resp.GetInSync():
		return 1
	default:
		return 0
	}
}

// printInspectReport renders findings grouped by severity, worst first.
//
// Grouped rather than listed in diff order because the three severities are
// three different conversations: a mismatch is somebody having changed the
// database by hand, an addition is usually the next apply, and a removal is a
// decision about data. Interleaving them buries the first in the third.
func printInspectReport(resp *adminpb.InspectSchemaResponse) {
	cliout.Header(os.Stdout, "inspect")
	if resp.GetInSync() {
		cliout.Successf("in sync — the database matches the declaration")
	} else {
		cliout.Field(os.Stdout, "findings", fmt.Sprintf("%d", len(resp.GetDrift())))
		if n := resp.GetMismatchCount(); n > 0 {
			cliout.Field(os.Stdout, "mismatch", cliout.Brass(fmt.Sprintf("%d", n)))
		}
	}

	byLevel := map[string][]*adminpb.AdoptDriftItem{}
	for _, d := range resp.GetDrift() {
		byLevel[d.GetSeverity()] = append(byLevel[d.GetSeverity()], d)
	}
	for _, level := range []string{"mismatch", "removal", "addition"} {
		items := byLevel[level]
		if len(items) == 0 {
			continue
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].GetEntityId() != items[j].GetEntityId() {
				return items[i].GetEntityId() < items[j].GetEntityId()
			}
			return items[i].GetField() < items[j].GetField()
		})
		fmt.Println()
		cliout.Header(os.Stdout, level)
		for _, d := range items {
			style := "muted"
			if level == "mismatch" {
				style = "warn"
			}
			cliout.Row(os.Stdout, style,
				cliout.Bold(d.GetEntityId()+"/"+d.GetField()), d.GetDetail())
		}
		// Unknown severities would otherwise vanish. Same failure the
		// destructive bucket had in every three-way client decode.
		delete(byLevel, level)
	}
	for level, items := range byLevel {
		fmt.Println()
		cliout.Header(os.Stdout, level)
		for _, d := range items {
			cliout.Row(os.Stdout, "muted",
				cliout.Bold(d.GetEntityId()+"/"+d.GetField()), d.GetDetail())
		}
	}

	if w := resp.GetWarnings(); len(w) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "not checked")
		for _, s := range w {
			cliout.Row(os.Stdout, "muted", "", s)
		}
	}
}
