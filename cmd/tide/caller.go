// tide caller — operator commands for managing registered callers.
//
//	tide caller alias list <caller>
//	tide caller alias add  <caller> <alias>
//	tide caller alias rm   <caller> <alias>
//
// Aliases let a cert's CN satisfy `visible_to` predicates declared for
// other identity names — PostgreSQL-roles / AD-SID / DNS-CNAME pattern.
// Operator-only RPCs gated by CAPABILITY_OPERATOR at the gRPC
// layer; the CLI delegates to the same admin service the console uses.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

func cmdCaller(args []string) int {
	if len(args) < 1 {
		printCallerUsage()
		return 2
	}
	switch args[0] {
	case "alias":
		return cmdCallerAlias(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide caller: unknown subcommand %q\n", args[0])
		printCallerUsage()
		return 2
	}
}

func printCallerUsage() {
	fmt.Fprintln(os.Stderr, "usage: tide caller alias list <caller>")
	fmt.Fprintln(os.Stderr, "       tide caller alias add  <caller> <alias>")
	fmt.Fprintln(os.Stderr, "       tide caller alias rm   <caller> <alias>")
}

func cmdCallerAlias(args []string) int {
	if len(args) < 1 {
		printCallerUsage()
		return 2
	}
	switch args[0] {
	case "list":
		return cmdCallerAliasList(args[1:])
	case "add":
		return cmdCallerAliasAdd(args[1:])
	case "rm", "remove":
		return cmdCallerAliasRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "tide caller alias: unknown subcommand %q\n", args[0])
		printCallerUsage()
		return 2
	}
}

func cmdCallerAliasList(args []string) int {
	fs := flag.NewFlagSet("caller alias list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: tide caller alias list <caller>")
		return 2
	}
	caller := fs.Arg(0)

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	resp, err := client.GetCallerAliases(ctx, &adminpb.GetCallerAliasesRequest{Caller: caller})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}

	if len(resp.GetAliases()) == 0 {
		fmt.Printf("%s: no aliases\n", resp.GetCaller())
		return 0
	}
	fmt.Printf("%s aliases:\n", resp.GetCaller())
	for _, a := range resp.GetAliases() {
		fmt.Printf("  %s\n", a)
	}
	return 0
}

func cmdCallerAliasAdd(args []string) int {
	fs := flag.NewFlagSet("caller alias add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: tide caller alias add <caller> <alias>")
		return 2
	}
	caller, alias := fs.Arg(0), fs.Arg(1)
	return cmdCallerAliasMutate(*configPath, *timeout, caller, func(existing []string) []string {
		// Insertion-order-preserving append + dedup, then normalize sort.
		for _, a := range existing {
			if a == alias {
				return existing
			}
		}
		out := append([]string{}, existing...)
		out = append(out, alias)
		sort.Strings(out)
		return out
	})
}

func cmdCallerAliasRemove(args []string) int {
	fs := flag.NewFlagSet("caller alias rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: tide caller alias rm <caller> <alias>")
		return 2
	}
	caller, alias := fs.Arg(0), fs.Arg(1)
	return cmdCallerAliasMutate(*configPath, *timeout, caller, func(existing []string) []string {
		out := make([]string, 0, len(existing))
		for _, a := range existing {
			if a != alias {
				out = append(out, a)
			}
		}
		return out
	})
}

// cmdCallerAliasMutate is the shared add/remove core. Fetches the
// current list, applies the supplied transform, and sends a single
// SetCallerAliases call. Read-modify-write isn't atomic against
// concurrent operators — the SetCallerAliases call replaces the
// whole list, so the last writer wins. The server-side normalize
// (dedup, validate) catches any divergence the transform missed.
func cmdCallerAliasMutate(configPath string, timeout time.Duration, caller string, transform func([]string) []string) int {
	cfg, err := loadPCConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}
	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Fetch current.
	cur, err := client.GetCallerAliases(ctx, &adminpb.GetCallerAliasesRequest{Caller: caller})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}

	updated := transform(cur.GetAliases())

	// Send the updated set.
	resp, err := client.SetCallerAliases(ctx, &adminpb.SetCallerAliasesRequest{
		Caller: caller, Aliases: updated,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "tide: %v\n", err)
		return 1
	}

	if len(resp.GetAliases()) == 0 {
		fmt.Printf("%s: aliases cleared\n", resp.GetCaller())
		return 0
	}
	fmt.Printf("%s aliases (after update):\n", resp.GetCaller())
	for _, a := range resp.GetAliases() {
		fmt.Printf("  %s\n", a)
	}
	return 0
}
