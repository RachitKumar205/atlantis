package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
	"github.com/rachitkumar205/atlantis/internal/workspace"
)

// cmdInspect — exit codes:
//
//	0 — every caller's declaration matches the database
//	1 — outstanding work (additions / removals)
//	2 — mismatch: both sides exist and disagree
//	3 — operational error (parse / network / config)
//
// The same code map as `tide inspect`, deliberately. An operator comparing a
// deployment-wide run against a caller's own CI run should not have to
// translate.
//
// # Why this exists alongside `tide inspect`
//
// They answer the same question at different scopes. A caller can only submit
// files for its own namespace, so `tide inspect` sees its own tables. An
// operator asking "is this whole deployment consistent with what every caller
// declares" needs the union, which is what the workspace manifest already
// resolves for adopt and codegen.
//
// This is what an operator used `adopt --allow-drift` for: finding out. Adopt
// remains for the doing.
func cmdInspect(args []string) int {
	fs := flagSet("inspect")
	workspaceFile := fs.String("workspace", "atlantis.workspace.yaml", "Path to the workspace manifest.")
	workspaceCache := fs.String("workspace-cache", ".workspace-cache", "Cache directory for resolved git callers.")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	format := fs.String("format", "table", "Output format: table or json.")
	timeout := fs.Duration("timeout", 120*time.Second, "RPC timeout (introspection across a large schema can take a while).")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	w, err := workspace.Load(*workspaceFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl inspect:", err)
		return 3
	}
	resolved, err := w.Resolve(*workspaceCache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl inspect:", err)
		return 3
	}
	if len(resolved) == 0 {
		fmt.Fprintln(os.Stderr, "tidectl inspect: workspace has no callers")
		return 3
	}

	var subs []*adminpb.CallerSubmission
	for _, rc := range resolved {
		var files []*adminpb.SubmittedFile
		for _, abs := range rc.Files {
			data, err := os.ReadFile(abs)
			if err != nil {
				fmt.Fprintln(os.Stderr, "tidectl inspect: read", abs, err)
				return 3
			}
			rel, _ := filepath.Rel(rc.CloneRoot, abs)
			files = append(files, &adminpb.SubmittedFile{Path: rel, Content: data})
		}
		subs = append(subs, &adminpb.CallerSubmission{Caller: rc.Name, Files: files})
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client, err := dialAdmin(adminDialConfig{
		Endpoint: *endpoint,
		TLSCert:  *tlsCert,
		TLSKey:   *tlsKey,
		TLSCA:    *tlsCA,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl inspect:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	resp, err := client.InspectSchema(ctx, &adminpb.InspectSchemaRequest{Submissions: subs})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl inspect:", err)
		return 3
	}

	switch *format {
	case "json":
		if err := emitJSON(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl inspect:", err)
			return 3
		}
	case "table":
		printInspectReport(resp, subs)
	default:
		fmt.Fprintf(os.Stderr, "tidectl inspect: unknown --format %q (want table|json)\n", *format)
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

func printInspectReport(resp *adminpb.InspectSchemaResponse, subs []*adminpb.CallerSubmission) {
	callers := make([]string, 0, len(subs))
	for _, s := range subs {
		callers = append(callers, s.GetCaller())
	}
	sort.Strings(callers)

	cliout.Header(os.Stdout, "inspect")
	cliout.Field(os.Stdout, "callers", fmt.Sprintf("%v", callers))
	if resp.GetInSync() {
		cliout.Successf("in sync — every declaration matches the database")
		return
	}
	cliout.Field(os.Stdout, "findings", fmt.Sprintf("%d", len(resp.GetDrift())))
	if n := resp.GetMismatchCount(); n > 0 {
		cliout.Field(os.Stdout, "mismatch", cliout.Brass(fmt.Sprintf("%d", n)))
	}

	// Grouped worst-first. A mismatch means somebody changed the database by
	// hand; an addition is usually the next apply. Interleaving buries the
	// first in the second.
	byLevel := map[string][]*adminpb.AdoptDriftItem{}
	var order []string
	for _, d := range resp.GetDrift() {
		if _, seen := byLevel[d.GetSeverity()]; !seen {
			order = append(order, d.GetSeverity())
		}
		byLevel[d.GetSeverity()] = append(byLevel[d.GetSeverity()], d)
	}
	// Known severities first, in severity order; then anything this build has
	// never heard of, under its own name rather than dropped.
	known := []string{"mismatch", "removal", "addition"}
	var levels []string
	levels = append(levels, known...)
	for _, s := range order {
		if !containsStr(known, s) {
			levels = append(levels, s)
		}
	}

	for _, level := range levels {
		items := byLevel[level]
		if len(items) == 0 {
			continue
		}
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
	}

	if w := resp.GetWarnings(); len(w) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "not checked")
		for _, s := range w {
			cliout.Row(os.Stdout, "muted", "", s)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
