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

// cmdOwners — `tide owners`
//
// Prints every active entity and the caller that introduced it.
// Useful for answering "who owns this table?" without digging through
// version history.
func cmdOwners(args []string) int {
	fs := flag.NewFlagSet("owners", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
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

	resp, err := client.GetEntityOwners(ctx, &adminpb.GetEntityOwnersRequest{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide owners:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tide owners:", err)
			return 3
		}
	case "table":
		printOwnersTable(resp)
	default:
		fmt.Fprintf(os.Stderr, "tide owners: unknown --format %q\n", *format)
		return 3
	}
	return 0
}

func printOwnersTable(resp *adminpb.GetEntityOwnersResponse) {
	if len(resp.GetOwners()) == 0 {
		fmt.Println(cliout.Grey("(no entities)"))
		return
	}
	fmt.Printf("%-32s %-16s %-8s %s\n",
		cliout.Bold("ENTITY"), cliout.Bold("OWNER"),
		cliout.Bold("SINCE"), cliout.Bold("FIELDS"))
	for _, o := range resp.GetOwners() {
		fmt.Printf("%-32s %-16s v%-7d %d\n",
			cliout.Cyan(o.GetEntityId()), o.GetIntroducedBy(), o.GetIntroducedAt(), o.GetFieldCount())
	}
}
