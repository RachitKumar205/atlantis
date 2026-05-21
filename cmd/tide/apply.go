package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SubmittedFile mirrors internal/server/admin.SubmittedFile. We don't
// import the server package because the CLI must stay a leaf module (no
// transitive deps on pgx, etc.). The JSON envelope is shared on the wire.
type SubmittedFile struct {
	Path    string `json:"Path"`
	Content []byte `json:"Content"`
}

type planRequest struct {
	Caller string          `json:"Caller"`
	Files  []SubmittedFile `json:"Files"`
}

type impactEntry struct {
	Caller   string `json:"Caller"`
	Affected bool   `json:"Affected"`
	Detail   string `json:"Detail"`
}

type planResponse struct {
	PlanID         string        `json:"PlanID"`
	Class          string        `json:"Class"`
	UpSQL          string        `json:"UpSQL"`
	DownSQL        string        `json:"DownSQL"`
	ImpactReport   []impactEntry `json:"ImpactReport"`
	ParseErrors    []string      `json:"ParseErrors"`
	BreakingDetail []string      `json:"BreakingDetail"`
}

type applyRequest struct {
	Caller string          `json:"Caller"`
	PlanID string          `json:"PlanID"`
	UpSQL  string          `json:"UpSQL"`
	Files  []SubmittedFile `json:"Files"`
}

type applyResponse struct {
	AppliedAt string `json:"AppliedAt"`
}

// Exit codes:
//   0 — plan applied (or dry-run / no changes).
//   1 — backfill required.
//   2 — breaking changes; need a atlantis PR.
//   3 — operational error (parse error, network failure, etc).
//
// cmdApply is the main user touchpoint for tide apply.
func cmdApply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	backfill := fs.String("backfill", "", "Backfill SQL file (required for backfill_required plans)")
	dryRun := fs.Bool("dry-run", false, "Plan only; do not apply")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	noPull := fs.Bool("no-pull", false, "Skip the pre-apply `tide pull` refresh of .tide-cache/")
	if err := fs.Parse(args); err != nil {
		return 3
	}

	cfg, err := loadPCConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
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

	// Refresh the local merged-schema cache so cross-caller references the
	// CLI is about to validate (e.g., `references vendor.Product.id` from a
	// new backend entity) resolve against the latest server-side state. A
	// network failure here is non-fatal; the server's planning RPC has the
	// definitive view.
	if !*noPull {
		pullBeforeApply(ctx, cfg)
	}

	client, err := dial(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide:", err)
		return 3
	}
	defer client.Close()

	var planResp planResponse
	err = client.invoke(ctx, "/atlantis.admin.v1.Admin/PlanSchema",
		planRequest{Caller: cfg.Caller, Files: files}, &planResp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", err)
		return 3
	}

	// Print parse / lower errors and bail. The server bundles DSL parse
	// errors and IR-lowering errors (missing FK targets, etc.) into the
	// same field; the message distinguishes them so a "references unknown
	// entity vendor.Foo" error doesn't send you hunting for syntax issues
	// when really data-pipeline just hasn't run `tide apply` yet.
	if len(planResp.ParseErrors) > 0 {
		fmt.Fprintln(os.Stderr, "tide: schema validation failed:")
		for _, e := range planResp.ParseErrors {
			fmt.Fprintln(os.Stderr, "  ", e)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "If errors mention 'references unknown entity vendor.X' or 'consumer.X',")
		fmt.Fprintln(os.Stderr, "the *other* caller hasn't registered its .atl files yet — run `tide apply`")
		fmt.Fprintln(os.Stderr, "from that caller's repo first.")
		return 3
	}

	printImpactReport(planResp)

	switch planResp.Class {
	case "additive":
		if *dryRun {
			fmt.Println("tide: additive plan; would apply (--dry-run set)")
			return 0
		}
		return doApply(ctx, client, cfg, planResp, files)

	case "backfill_required":
		if *backfill == "" {
			fmt.Fprintln(os.Stderr, "tide: this change is backfill-required.")
			fmt.Fprintln(os.Stderr, "    Write the backfill SQL and re-run with --backfill <file>.")
			return 1
		}
		// The server applies a single .up.sql; the backfill is not woven
		// into the migration. We surface a clear error so the operator
		// runs it manually.
		fmt.Fprintln(os.Stderr, "tide: --backfill is accepted but the v0.1 server")
		fmt.Fprintln(os.Stderr, "    does not yet splice backfill SQL into the migration.")
		fmt.Fprintln(os.Stderr, "    Apply your backfill SQL manually, then re-run tide apply.")
		return 1

	case "cross_caller_breaking":
		fmt.Fprintln(os.Stderr, "tide: this change is breaking other callers:")
		for _, d := range planResp.BreakingDetail {
			fmt.Fprintln(os.Stderr, "  ", d)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "tide: open a PR in the atlantis repo to coordinate the change.")
		return 2

	default:
		fmt.Fprintf(os.Stderr, "tide: unknown plan class %q\n", planResp.Class)
		return 3
	}
}

func doApply(ctx context.Context, client *adminClient, cfg *tideConfig, plan planResponse, files []SubmittedFile) int {
	var applyResp applyResponse
	err := client.invoke(ctx, "/atlantis.admin.v1.Admin/ApplyMigration",
		applyRequest{
			Caller: cfg.Caller,
			PlanID: plan.PlanID,
			UpSQL:  plan.UpSQL,
			Files:  files,
		}, &applyResp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide apply:", err)
		return 3
	}
	fmt.Printf("tide: ✓ applied at %s\n", applyResp.AppliedAt)
	if cfg.OutputDir != "" {
		fmt.Printf("tide: regenerate the typed client under %s with `buf generate` (v0.2 will run this automatically)\n",
			cfg.OutputDir)
	}
	return 0
}

func printImpactReport(p planResponse) {
	if len(p.ImpactReport) == 0 {
		return
	}
	fmt.Println("tide: impact report:")
	for _, e := range p.ImpactReport {
		mark := " "
		if e.Affected {
			mark = "*"
		}
		fmt.Printf("  %s %-24s %s\n", mark, e.Caller, e.Detail)
	}
	fmt.Println()
}

// collectPCFiles walks every schema path and reads every .atl file. Paths
// are stored relative to the caller's repo root so the server's error
// messages are useful in the caller's context.
func collectPCFiles(paths []string) ([]SubmittedFile, error) {
	var out []SubmittedFile
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".atl" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, SubmittedFile{Path: path, Content: data})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}
