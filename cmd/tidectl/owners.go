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

// cmdOwners — `tidectl owners`
func cmdOwners(args []string) int {
	fs := flagSet("owners")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	format := fs.String("format", "table", "Output format: table or json")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := cliflag.ParseNoArgs(fs, args); err != nil {
		return 3
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint, TLSCert: *tlsCert, TLSKey: *tlsKey, TLSCA: *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl owners:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.GetEntityOwners(ctx, &adminpb.GetEntityOwnersRequest{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl owners:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl owners:", err)
			return 3
		}
	case "table":
		printOwnersTable(resp)
	default:
		fmt.Fprintf(os.Stderr, "tidectl owners: unknown --format %q\n", *format)
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
			cliout.Cyan(o.GetEntityId()), o.GetIntroducedBy(),
			o.GetIntroducedAt(), o.GetFieldCount())
	}
}
