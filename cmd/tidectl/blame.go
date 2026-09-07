package main

import (
	"context"
	"fmt"
	"os"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliflag"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// cmdBlame — `tidectl blame <entity-id>`
func cmdBlame(args []string) int {
	fs := flagSet("blame")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	pos, err := cliflag.Parse(fs, args)
	if err != nil {
		return 3
	}
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "usage: tidectl blame <entity-id>")
		return 2
	}
	entityID := pos[0]

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint, TLSCert: *tlsCert, TLSKey: *tlsKey, TLSCA: *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl blame:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetEntityLineage(ctx, &adminpb.GetEntityLineageRequest{EntityId: entityID})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl blame:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl blame:", err)
			return 3
		}
	case "table":
		printBlameTable(entityID, resp)
	default:
		fmt.Fprintf(os.Stderr, "tidectl blame: unknown --format %q\n", *format)
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
