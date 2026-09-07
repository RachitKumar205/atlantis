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
	if err := cliflag.ParseNoArgs(fs, args); err != nil {
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
	// cliout.Pad, not %-Ns: fmt pads by bytes, and the colour wrappers here add
	// invisible ones, so these columns never lined up when colour was on.
	fmt.Println(cliout.Pad(cliout.Bold("ENTITY"), 33) +
		cliout.Pad(cliout.Bold("OWNER"), 17) +
		cliout.Pad(cliout.Bold("SINCE"), 9) + cliout.Bold("FIELDS"))
	for _, o := range resp.GetOwners() {
		fmt.Println(cliout.Pad(cliout.Cyan(o.GetEntityId()), 33) +
			cliout.Pad(o.GetIntroducedBy(), 17) +
			cliout.Pad(fmt.Sprintf("v%d", o.GetIntroducedAt()), 9) +
			fmt.Sprintf("%d", o.GetFieldCount()))
	}
}
