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

// cmdBlame — `tide blame <entity-id>`
//
// Shows per-field provenance for an entity: who introduced each field,
// who last modified it, and which schema version those events correspond
// to. Mirrors `git blame` for the schema.
func cmdBlame(args []string) int {
	fs := flag.NewFlagSet("blame", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 3
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: tide blame <entity-id>")
		return 2
	}
	entityID := fs.Arg(0)

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

	resp, err := client.GetEntityLineage(ctx, &adminpb.GetEntityLineageRequest{EntityId: entityID})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide blame:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide blame:", err)
			return 3
		}
	case "table":
		printBlameTable(entityID, resp)
	default:
		fmt.Fprintf(os.Stderr, "tide blame: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printBlameTable(entityID string, resp *adminpb.GetEntityLineageResponse) {
	if len(resp.GetEntries()) == 0 {
		cliout.Errorf("no lineage found for %s", entityID)
		return
	}
	fmt.Printf("%s %s\n\n", cliout.Bold("Blame:"), cliout.Cyan(entityID))
	fmt.Printf("%-24s %-16s %-8s %-16s %-8s %s\n",
		cliout.Bold("FIELD"), cliout.Bold("INTRODUCED BY"),
		cliout.Bold("AT"), cliout.Bold("MODIFIED BY"),
		cliout.Bold("AT"), cliout.Bold("STATUS"))
	for _, e := range resp.GetEntries() {
		field := e.GetFieldName()
		if field == "" {
			field = cliout.Grey("(entity)")
		}
		status := cliout.Green("active")
		if e.RemovedAt != nil {
			status = cliout.Red(fmt.Sprintf("removed@v%d", e.GetRemovedAt()))
		}
		fmt.Printf("%-24s %-16s %-8d %-16s %-8d %s\n",
			field, e.GetIntroducedBy(), e.GetIntroducedAt(),
			e.GetLastModifiedBy(), e.GetLastModifiedAt(), status)
	}
}
