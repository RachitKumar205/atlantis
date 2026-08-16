package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	generate := fs.String("generate", "", "Write .atl for tables no declaration mentions into this directory")
	pgSchemas := fs.String("schemas", "", "With --generate: comma-separated Postgres schemas to search (default: all non-system)")
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

	// --generate is the one subcommand that is SUPPOSED to run in a repo with
	// no .atl files: it exists to write the first ones. Both the walk error
	// and the empty result are therefore tolerated on that path.
	//
	// They were not, and the guard sat above the generate dispatch, so
	// `tide inspect --generate` refused with "no .atl files found" in exactly
	// the repo it was built for. The server side always handled it —
	// GenerateSchema leaves declaredIR nil when there are no submissions, and
	// DiscoverTables documents nil as "nothing is declared yet".
	generating := *generate != ""

	files, err := collectPCFiles(cfg.SchemaPaths)
	if err != nil {
		// A schema directory that does not exist yet is not an error when the
		// point of the command is to create its contents.
		if !generating || !errors.Is(err, iofs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "tide:", err)
			return 3
		}
		files = nil
	}
	if len(files) == 0 && !generating {
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

	if *generate != "" {
		return runGenerate(ctx, client, cfg, files, *generate, *pgSchemas)
	}

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

// runGenerate writes .atl for tables no declaration mentions.
//
// One file per entity. A single file would be one merge conflict for every
// pair of people onboarding different parts of a schema, and a customer
// reviewing forty generated tables wants to delete the ones they do not want
// by deleting files.
//
// Never overwrites. A generated file the customer has since edited is the
// worst thing this command could destroy, and "re-run to refresh" is not worth
// that risk — they can delete a file to regenerate it.
// generatedFileName turns a server-supplied entity name into a filename, or
// refuses.
//
// An entity name reaching here came from introspecting a table name, and a
// Postgres identifier is only constrained by what fits in quotes. The
// accepted shape is what dsl.Parse would accept as an entity name anyway —
// letters, digits and underscore — so a name this rejects could not have
// produced a usable declaration either.
func generatedFileName(entity string) (string, error) {
	if entity == "" {
		return "", errors.New("skipped: the server returned an entity with no name")
	}
	for _, r := range entity {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return "", fmt.Errorf("skipped: entity name %q is not a safe filename "+
				"(only letters, digits and underscore) — rename the table or "+
				"declare it by hand", entity)
		}
	}
	return strings.ToLower(entity) + ".atl", nil
}

func runGenerate(ctx context.Context, client *adminClient, cfg *tideConfig,
	files []*adminpb.SubmittedFile, outDir, pgSchemas string,
) int {
	var schemas []string
	for _, s := range strings.Split(pgSchemas, ",") {
		if s = strings.TrimSpace(s); s != "" {
			schemas = append(schemas, s)
		}
	}

	req := &adminpb.GenerateSchemaRequest{
		Namespace: cfg.Caller,
		Schemas:   schemas,
	}
	// Omitted entirely when there is nothing declared yet, rather than sent as
	// a submission carrying an empty file list. GenerateSchema branches on
	// len(subs) > 0 to decide whether to compare against a declaration at all,
	// and a submission with no files is not the same statement as no
	// submission — the first asks it to parse an empty caller.
	if len(files) > 0 {
		req.Submissions = []*adminpb.CallerSubmission{
			{Caller: cfg.Caller, Files: files},
		}
	}

	resp, err := client.GenerateSchema(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide inspect --generate:", err)
		return 3
	}

	cliout.Header(os.Stdout, "generate")
	if len(resp.GetEntities()) == 0 {
		cliout.Successf("every table is already declared — nothing to generate")
		printGenerateNotes(resp)
		return 0
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "tide inspect --generate:", err)
		return 3
	}

	written, skipped := 0, 0
	for _, e := range resp.GetEntities() {
		// The entity name is server-supplied and derived from a table name, so
		// it is not a filename until this says it is. Postgres accepts almost
		// anything inside quoted identifiers — `CREATE TABLE "../../etc/x"` is
		// legal — and filepath.Join CLEANS the result, so a name containing
		// ".." silently resolves outside outDir and os.WriteFile follows it.
		//
		// Refusing beats sanitising: a mangled filename would leave the
		// customer with a file whose name does not match the entity inside it.
		base, err := generatedFileName(e.GetEntityName())
		if err != nil {
			cliout.Row(os.Stdout, "warn", cliout.Bold(e.GetTable()), err.Error())
			skipped++
			continue
		}
		path := filepath.Join(outDir, base)
		if _, err := os.Stat(path); err == nil {
			cliout.Row(os.Stdout, "muted", cliout.Bold(path), "exists — left alone")
			skipped++
			continue
		}
		if err := os.WriteFile(path, []byte(e.GetAtl()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "tide inspect --generate: write", path, err)
			return 3
		}
		cliout.Row(os.Stdout, "muted", cliout.Bold(path), e.GetTable())
		written++
	}

	fmt.Println()
	cliout.Field(os.Stdout, "written", fmt.Sprintf("%d", written))
	if skipped > 0 {
		cliout.Field(os.Stdout, "kept", fmt.Sprintf("%d (already on disk)", skipped))
	}
	printGenerateNotes(resp)

	fmt.Println()
	cliout.Warnf("Review before committing. Entity names are a proposal, and they " +
		"become your generated API — renaming one later is a breaking change.")
	return 0
}

// printGenerateNotes renders what was skipped and what could not be checked.
//
// Both are printed even when empty-handed elsewhere. A customer told "we found
// 40 tables" who had 41 has no way to notice, so the reason each one was
// dropped goes on screen rather than into a log.
func printGenerateNotes(resp *adminpb.GenerateSchemaResponse) {
	if s := resp.GetSkipped(); len(s) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "not generated")
		for _, line := range s {
			cliout.Row(os.Stdout, "warn", "", line)
		}
	}
	if w := resp.GetWarnings(); len(w) > 0 {
		fmt.Println()
		cliout.Header(os.Stdout, "not checked")
		for _, line := range w {
			cliout.Row(os.Stdout, "muted", "", line)
		}
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
