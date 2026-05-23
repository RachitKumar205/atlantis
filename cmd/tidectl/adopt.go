package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cliout"
	"github.com/rachitkumar205/atlantis/internal/workspace"
)

// SubmittedFile mirrors internal/server/admin.SubmittedFile on the wire.
type SubmittedFile struct {
	Path    string `json:"Path"`
	Content []byte `json:"Content"`
}

// callerSubmission mirrors internal/server/admin.CallerSubmission.
type callerSubmission struct {
	Caller string          `json:"Caller"`
	Files  []SubmittedFile `json:"Files"`
}

type adoptBaselineRequest struct {
	Submissions []callerSubmission `json:"Submissions"`
	AllowDrift  bool               `json:"AllowDrift"`
	AdoptedBy   string             `json:"AdoptedBy"`
}

type adoptDriftItem struct {
	EntityID string `json:"entity_id"`
	Field    string `json:"field,omitempty"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Detail   string `json:"detail,omitempty"`
}

type adoptBaselineResponse struct {
	CheckpointWritten bool             `json:"CheckpointWritten"`
	AlreadyAdopted    bool             `json:"AlreadyAdopted"`
	Drift             []adoptDriftItem `json:"Drift,omitempty"`
	Warnings          []string         `json:"Warnings,omitempty"`
}

// cmdAdopt — exit codes:
//
//	0 — clean adopt or clean drift-accepted (CheckpointWritten=true)
//	1 — drift detected, baseline refused (CheckpointWritten=false)
//	3 — operational error (parse / network / config)
//
// Reads atlantis.workspace.yaml (or --workspace path), resolves every
// caller, batches them into one AdoptBaseline RPC. Multi-caller atomic
// on the server side — either every caller baselines or none do.
func cmdAdopt(args []string) int {
	fs := flagSet("adopt")
	workspaceFile := fs.String("workspace", "atlantis.workspace.yaml", "Path to the workspace manifest.")
	workspaceCache := fs.String("workspace-cache", ".workspace-cache", "Cache directory for resolved git callers.")
	endpoint := fs.String("endpoint", envDefault("ATL_ENDPOINT", "localhost:9090"), "Admin gRPC endpoint (host:port).")
	tlsCert := fs.String("tls-cert", os.Getenv("ATL_TLS_CERT"), "Client TLS cert (PEM).")
	tlsKey := fs.String("tls-key", os.Getenv("ATL_TLS_KEY"), "Client TLS key (PEM).")
	tlsCA := fs.String("tls-ca", os.Getenv("ATL_TLS_CA"), "Server CA bundle (PEM).")
	allowDrift := fs.Bool("allow-drift", false, "Baseline even when introspection finds drift. Records the drift report into atlantis.adopt_history for later audit.")
	format := fs.String("format", "table", "Output format: table or json.")
	timeout := fs.Duration("timeout", 120*time.Second, "RPC timeout (introspection across a large schema can take a while).")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	w, err := workspace.Load(*workspaceFile)
	if err != nil {
		// Common footgun: operator runs tidectl adopt from the atlantis
		// repo where atlantis.workspace.yaml is the empty prod template,
		// but the real manifest is the dev one. Point at the alternative.
		if _, statErr := os.Stat("atlantis.dev.yaml"); statErr == nil && *workspaceFile == "atlantis.workspace.yaml" {
			fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
			fmt.Fprintln(os.Stderr, "tidectl adopt: found atlantis.dev.yaml — re-run with `--workspace=atlantis.dev.yaml` to use it")
			return 3
		}
		fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
		return 3
	}
	resolved, err := w.Resolve(*workspaceCache)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
		return 3
	}
	if len(resolved) == 0 {
		fmt.Fprintln(os.Stderr, "tidectl adopt: workspace has no callers")
		return 3
	}

	var subs []callerSubmission
	for _, rc := range resolved {
		var files []SubmittedFile
		for _, abs := range rc.Files {
			data, err := os.ReadFile(abs)
			if err != nil {
				fmt.Fprintln(os.Stderr, "tidectl adopt: read", abs, err)
				return 3
			}
			rel, _ := filepath.Rel(rc.CloneRoot, abs)
			files = append(files, SubmittedFile{Path: rel, Content: data})
		}
		subs = append(subs, callerSubmission{Caller: rc.Name, Files: files})
	}

	principal := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		principal = u.Username
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
		fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
		return 3
	}
	defer func() { _ = client.Close() }()

	req := adoptBaselineRequest{
		Submissions: subs,
		AllowDrift:  *allowDrift,
		AdoptedBy:   principal,
	}
	var resp adoptBaselineResponse
	if err := client.invoke(ctx, "/atlantis.admin.v1.Admin/AdoptBaseline", req, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
		return 3
	}

	if *format == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "tidectl adopt:", err)
			return 3
		}
	} else {
		printAdoptReport(resp, *allowDrift, subs)
	}

	switch {
	case resp.AlreadyAdopted, resp.CheckpointWritten:
		return 0
	default:
		return 1
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func printAdoptReport(resp adoptBaselineResponse, allowDrift bool, subs []callerSubmission) {
	callers := make([]string, 0, len(subs))
	for _, s := range subs {
		callers = append(callers, s.Caller)
	}
	sort.Strings(callers)
	callerList := cliout.Bold(fmt.Sprintf("%v", callers))

	additions, removals, mismatches := bucketDrift(resp.Drift)

	switch {
	case resp.AlreadyAdopted:
		cliout.Successf("already baselined with this exact set of files for callers %s. No changes.", callerList)
	case resp.CheckpointWritten && len(resp.Drift) == 0:
		cliout.Successf("declared schema matches live DB. Checkpoint recorded for callers %s.", callerList)
	case resp.CheckpointWritten && len(mismatches) > 0:
		cliout.Warnf("baseline recorded for callers %s %s — review the disagreements below.", callerList, cliout.Yellow("WITH ACCEPTED DRIFT"))
	case resp.CheckpointWritten:
		cliout.Successf("baseline recorded for callers %s. %d outstanding migration(s) below.", callerList, len(additions)+len(removals))
	default:
		cliout.Errorf("schema disagreement found across callers %s — refusing to baseline.", callerList)
	}

	if len(additions) > 0 {
		fmt.Println()
		fmt.Printf("%s %s %s\n",
			cliout.Green("+"),
			cliout.Bold(fmt.Sprintf("%d", len(additions))),
			cliout.Bold("outstanding addition(s) — declared in .atl, not in live DB:"))
		fmt.Println(cliout.Grey("  (will be created by `tide apply`; not blocking adopt)"))
		printDriftSection(additions, cliout.Green("+"))
	}

	if len(removals) > 0 {
		fmt.Println()
		fmt.Printf("%s %s %s\n",
			cliout.Cyan("-"),
			cliout.Bold(fmt.Sprintf("%d", len(removals))),
			cliout.Bold("outstanding removal(s) — present in live DB, not declared:"))
		fmt.Println(cliout.Grey("  (would be dropped by `tide apply` if the omission is intentional; not blocking adopt)"))
		printDriftSection(removals, cliout.Cyan("-"))
	}

	if len(mismatches) > 0 {
		fmt.Println()
		fmt.Printf("%s %s %s\n",
			cliout.Red("~"),
			cliout.Bold(fmt.Sprintf("%d", len(mismatches))),
			cliout.Red(cliout.Bold("schema disagreement(s) — .atl and live DB describe the same field differently:")))
		fmt.Println(cliout.Grey("  (these BLOCK adopt unless --allow-drift; resolve by editing .atl or fixing live DB)"))
		printDriftSection(mismatches, cliout.Red("~"))
	}

	if len(mismatches) > 0 && !resp.CheckpointWritten && !allowDrift {
		fmt.Println()
		fmt.Println(cliout.Bold("Resolve by either:"))
		fmt.Printf("  %s edit the .atl files to match the live DB, then re-run %s\n", cliout.Cyan("→"), cliout.Bold("tidectl adopt"))
		fmt.Printf("  %s run migrations against the live DB to match the .atl, then re-run %s\n", cliout.Cyan("→"), cliout.Bold("tidectl adopt"))
		fmt.Printf("  %s re-run with %s to baseline anyway (recorded in %s)\n", cliout.Cyan("→"), cliout.Bold("--allow-drift"), cliout.Grey("atlantis.adopt_history"))
	}

	if len(resp.Warnings) > 0 {
		fmt.Println()
		fmt.Printf("%s %s\n", cliout.Yellow("Advisory warnings"), cliout.Grey("(not blocking):"))
		for _, w := range resp.Warnings {
			fmt.Printf("  %s %s\n", cliout.Grey("·"), w)
		}
	}
}

// bucketDrift partitions the wire-side drift slice into the three
// severity buckets the renderer treats differently. Inputs are
// already-severity-stamped by the server.
func bucketDrift(drift []adoptDriftItem) (additions, removals, mismatches []adoptDriftItem) {
	for _, d := range drift {
		switch d.Severity {
		case "addition":
			additions = append(additions, d)
		case "removal":
			removals = append(removals, d)
		default:
			mismatches = append(mismatches, d)
		}
	}
	return
}

func printDriftSection(items []adoptDriftItem, bullet string) {
	byEntity := make(map[string][]adoptDriftItem)
	for _, d := range items {
		byEntity[d.EntityID] = append(byEntity[d.EntityID], d)
	}
	entities := make([]string, 0, len(byEntity))
	for e := range byEntity {
		entities = append(entities, e)
	}
	sort.Strings(entities)
	for _, e := range entities {
		fmt.Printf("  %s\n", cliout.Bold(e))
		for _, d := range byEntity[e] {
			label := cliout.Yellow(d.Kind)
			if d.Field != "" {
				label = fmt.Sprintf("%s/%s", cliout.Cyan(d.Field), cliout.Yellow(d.Kind))
			}
			if d.Detail != "" {
				fmt.Printf("    %s %-40s %s\n", bullet, label, cliout.Grey(d.Detail))
			} else {
				fmt.Printf("    %s %s\n", bullet, label)
			}
		}
	}
}
