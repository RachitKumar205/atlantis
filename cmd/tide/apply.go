package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cliout"
)

// planClassName renders a PlanClass for humans, stripping the enum prefix the
// wire carries. "PLAN_CLASS_BACKFILL_REQUIRED" is the right thing on the wire
// and the wrong thing in a terminal.
func planClassName(c adminpb.PlanClass) string {
	return strings.ToLower(strings.TrimPrefix(c.String(), "PLAN_CLASS_"))
}

// Exit codes:
//
//	0 — applied (or --dry-run on an additive plan, or no changes).
//	1 — backfill required and --backfill was not passed.
//	2 — blocked on a human: the change policy requires an approval that has not
//	    been given, or the request was rejected. Not an operational failure —
//	    the gate worked.
//	3 — operational error (parse error, network failure, unknown plan class).
//	4 — --dry-run only, on a class `tide plan` would exit 4 for.
//
// apply and plan keep separate code maps. plan classifies and reports; apply
// submits and reports what the server decided, and waiting on a reviewer is an
// outcome plan cannot produce.
func cmdApply(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "tide.yaml", "Path to tide.yaml")
	backfill := fs.Bool("backfill", false, "Kick off the declarative backfill flow for a backfill_required plan (calls BeginBackfillPlan; monitor with `tide backfill status`)")
	dryRun := fs.Bool("dry-run", false, "Plan only; do not apply")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC timeout")
	noPull := fs.Bool("no-pull", false, "Skip the pre-apply `tide pull` refresh of .tide-cache/")
	// Opt-in, and off by default on purpose. tide runs on CI runners, and a
	// polling default would hold one for as long as a review takes — which is
	// however long the reviewer is at lunch. The default is to file the request,
	// exit, and let the pipeline be re-run once somebody has decided.
	waitForApproval := fs.Duration("wait-for-approval", 0,
		"Wait up to this long for a human to approve the change, retrying the apply (e.g. 30m). Default: do not wait")
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

	// --timeout bounds one RPC; --wait-for-approval bounds how long we keep
	// retrying one. They are different clocks and this used to conflate them:
	// the whole command ran under a single 30s deadline, so
	// `--wait-for-approval=30m` expired after 30 seconds and reported the
	// original refusal. The flag could never wait, whatever it was set to.
	//
	// The budget is the wait PLUS one timeout, so the attempt issued just
	// before the deadline still gets a full RPC rather than being cut off
	// mid-flight.
	ctx, cancel := context.WithTimeout(context.Background(), commandBudget(*timeout, *waitForApproval))
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
	defer func() { _ = client.Close() }()

	planResp, err := client.PlanSchema(ctx, &adminpb.PlanSchemaRequest{
		Caller: cfg.Caller, Files: files,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide plan:", explainAuthz(err))
		return 3
	}

	// Print parse / lower errors and bail. The server bundles DSL parse
	// errors and IR-lowering errors (missing FK targets, etc.) into the
	// same field; the message distinguishes them so a "references unknown
	// entity vendor.Foo" error doesn't send you hunting for syntax issues
	// when really another caller just hasn't run `tide apply` yet.
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

	switch planResp.GetClass() {
	case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
		if *dryRun {
			fmt.Println("tide: additive plan; would apply (--dry-run set)")
			return 0
		}
		return doApply(ctx, client, cfg, planResp, files, *waitForApproval, *timeout)

	case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
		// --dry-run is checked BEFORE --backfill, because the backfill arm is
		// the one that acts. Every other arm honoured the flag and this one did
		// not, so `tide apply --dry-run --backfill` called BeginBackfillPlan
		// and started the backfill — the flag whose entire promise is "do not
		// apply" performing the longest-running write in the product.
		if *dryRun {
			fmt.Println("tide: backfill-required plan; --dry-run set, no backfill started")
			if len(planResp.GetBackfillFields()) > 0 {
				fmt.Fprintln(os.Stderr, "    Would backfill:")
				for _, f := range planResp.GetBackfillFields() {
					fmt.Fprintf(os.Stderr, "      %s.%s ← %s\n", f.GetEntityId(), f.GetField(), f.GetExpression())
				}
			}
			return exitCodeForClass(planResp.GetClass())
		}
		if !*backfill {
			fmt.Fprintln(os.Stderr, "tide: this change is backfill-required.")
			if len(planResp.GetBackfillFields()) > 0 {
				fmt.Fprintln(os.Stderr, "    Declared backfills:")
				for _, f := range planResp.GetBackfillFields() {
					fmt.Fprintf(os.Stderr, "      %s.%s ← %s\n", f.GetEntityId(), f.GetField(), f.GetExpression())
				}
				fmt.Fprintln(os.Stderr, "    Re-run with --backfill to kick off the chunked backfill.")
			} else {
				fmt.Fprintln(os.Stderr, "    No `backfill \"<expr>\"` modifiers declared on the relevant fields —")
				fmt.Fprintln(os.Stderr, "    add one in your .atl files and re-plan, or apply the backfill out of band.")
			}
			return 1
		}
		return doBeginBackfill(ctx, client, cfg, planResp, files)

	case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
		fmt.Fprintln(os.Stderr, "tide: this change is breaking other callers:")
		for _, d := range planResp.GetBreakingDetail() {
			fmt.Fprintln(os.Stderr, "  ", d)
		}
		fmt.Fprintln(os.Stderr, "")
		if *dryRun {
			return exitCodeForClass(planResp.GetClass())
		}
		// Submitted rather than refused here, and that is the point of moving
		// the gate to the server. Whether this may apply is the deployment's
		// change policy to decide, not tide's — and submitting is also how the
		// request reaches the approval queue, so refusing locally would leave
		// nobody anything to approve.
		return doApply(ctx, client, cfg, planResp, files, *waitForApproval, *timeout)

	case adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE:
		// Its own arm, not folded into the breaking one. The two need different
		// things from the reader: breaking means somebody else's code stops
		// working and is fixed by shipping their change first, while this means
		// rows go away and is fixed by deciding whether that is intended. The
		// detail list already distinguishes them — the server appends the
		// destructive entries with their parked/reaped note.
		fmt.Fprintln(os.Stderr, "tide: this change destroys data:")
		for _, d := range planResp.GetBreakingDetail() {
			fmt.Fprintln(os.Stderr, "  ", d)
		}
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "      Dropped objects are parked, not deleted, and reaped after the")
		fmt.Fprintln(os.Stderr, "      retention window — `tide parked` shows what is held and until when.")
		fmt.Fprintln(os.Stderr, "")
		if *dryRun {
			return exitCodeForClass(planResp.GetClass())
		}
		return doApply(ctx, client, cfg, planResp, files, *waitForApproval, *timeout)

	case adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:
		// Server marks the plan unparseable when pg_query_go validation on
		// any custom-query SQL body fails. Surface the actual failures so
		// the operator can fix the .atl file without spelunking the server logs.
		fmt.Fprintln(os.Stderr, "tide: plan is unparseable — custom-query SQL validation failed.")
		if len(planResp.GetCustomSqlErrors()) > 0 {
			fmt.Fprintln(os.Stderr, "")
			for _, e := range planResp.GetCustomSqlErrors() {
				fmt.Fprintln(os.Stderr, "  ", e)
			}
		}
		if len(planResp.ParseErrors) > 0 {
			fmt.Fprintln(os.Stderr, "")
			for _, e := range planResp.ParseErrors {
				fmt.Fprintln(os.Stderr, "  ", e)
			}
		}
		return 3

	default:
		// A class this binary does not know means the server is newer. Exiting
		// 3 rather than guessing is deliberate: the alternative is treating an
		// unrecognized class as benign and applying something whose risk this
		// tide cannot assess.
		fmt.Fprintf(os.Stderr, "tide: unknown plan class %q — upgrade tide to match this server\n",
			planResp.GetClass())
		return 3
	}
}

// doBeginBackfill submits the phase-split scripts + field driver list to
// the server's BeginBackfillPlan RPC. The server runs Phase 1 inline
// (additive parts + nullable ADD COLUMN), records the backfill_plan +
// backfill_field_state rows, and returns immediately. The background
// worker picks up the field rows and runs the chunked UPDATE loop;
// operators monitor with `tide backfill status`.
func doBeginBackfill(ctx context.Context, client *adminClient, cfg *tideConfig, plan *adminpb.PlanSchemaResponse, files []*adminpb.SubmittedFile) int {
	if len(plan.BackfillFields) == 0 {
		fmt.Fprintln(os.Stderr, "tide: --backfill set but no fields declare `backfill \"<expr>\"`.")
		fmt.Fprintln(os.Stderr, "    Add the modifier in your .atl files (see docs), re-plan, then re-run.")
		return 1
	}
	// Only the plan id and the files. The SQL the plan computed is not sent:
	// BeginBackfillPlanRequest has no field for it, and that absence is the fix
	// for an audit finding — the server used to execute req.PreBackfillUpSQL
	// verbatim, which made "may apply schema" mean "may run arbitrary SQL".
	//
	// What executes is codegen's output for the submitted .atl files. That is a
	// real narrowing — from any SQL to SQL the emitter will produce — and the
	// plan id now covers the files themselves too, since computePlanID hashes
	// their contents rather than their paths. The property that holds
	// regardless of either is that the request has no channel for raw SQL at
	// all.
	resp, err := client.BeginBackfillPlan(ctx, &adminpb.BeginBackfillPlanRequest{
		Caller: cfg.Caller,
		PlanId: plan.GetPlanId(),
		Files:  files,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tide backfill:", err)
		return 3
	}
	switch {
	case resp.GetAlreadyComplete():
		cliout.Successf("backfill already complete (plan-hash=%s)", cliout.Bold(resp.GetPlanHash()))
		return 0
	case resp.GetAlreadyRunning():
		cliout.Infof("backfill already in flight (plan-hash=%s)", cliout.Bold(resp.GetPlanHash()))
		fmt.Printf("      monitor with: %s %s\n", cliout.Bold("tide backfill status"), resp.GetPlanHash())
		return 0
	case resp.GetAccepted():
		cliout.Successf("backfill accepted (plan-hash=%s)", cliout.Bold(resp.GetPlanHash()))
		if msg := resp.GetMessage(); msg != "" {
			fmt.Printf("      %s\n", cliout.Grey(msg))
		}
		fmt.Printf("      monitor with: %s %s\n", cliout.Bold("tide backfill status"), resp.GetPlanHash())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "tide backfill: unexpected response: %+v\n", resp)
		return 3
	}
}

// commandBudget is how long the whole command may run.
//
// --timeout bounds ONE RPC; --wait-for-approval bounds how long we keep
// retrying one. Conflating them is what made --wait-for-approval inert: the
// command ran under a single 30s deadline, so `--wait-for-approval=30m`
// expired after thirty seconds and reported the original refusal. The flag
// could never wait, whatever it was set to, and the only symptom was a
// pipeline that gave up early and looked like the reviewer had been slow.
//
// The wait plus one timeout, so the attempt issued just before the wait
// deadline still gets a full RPC rather than being cut off mid-flight.
func commandBudget(rpcTimeout, wait time.Duration) time.Duration {
	if wait <= 0 {
		return rpcTimeout
	}
	return wait + rpcTimeout
}

func doApply(ctx context.Context, client *adminClient, cfg *tideConfig, plan *adminpb.PlanSchemaResponse, files []*adminpb.SubmittedFile, wait, rpcTimeout time.Duration) int {
	// No UpSQL. The plan's SQL was always documented as a drift check the server
	// never read, and ApplyMigrationRequest now has no field for it. The server
	// re-emits the DDL from these files against the checkpoint the hash pins,
	// so the statements that run are codegen's output for the submitted schema
	// rather than a string the client chose.
	req := &adminpb.ApplyMigrationRequest{
		Caller:         cfg.Caller,
		PlanId:         plan.GetPlanId(),
		Files:          files,
		CheckpointHash: plan.GetCheckpointHash(),
	}
	applyResp, err := client.ApplyMigration(ctx, req)
	if err != nil && status.Code(err) == codes.FailedPrecondition && wait > 0 {
		applyResp, err = waitForDecision(ctx, client, req, wait, rpcTimeout, err)
	}
	if err != nil {
		// FailedPrecondition is the change policy: this needs a human, or a
		// human already said no. It is not an operational failure, and reporting
		// it as one would have CI page somebody about a working gate.
		//
		// Branching on the code rather than the message, because the message is
		// written for a person and will be reworded.
		if status.Code(err) == codes.FailedPrecondition {
			fmt.Fprintln(os.Stderr, "tide apply:", status.Convert(err).Message())
			if wait == 0 {
				fmt.Fprintln(os.Stderr, "")
				fmt.Fprintln(os.Stderr, "      Re-run once it has been decided, or pass --wait-for-approval=30m")
				fmt.Fprintln(os.Stderr, "      to hold this process open until then.")
			}
			return 2
		}
		fmt.Fprintln(os.Stderr, "tide apply:", explainAuthz(err))
		return 3
	}
	cliout.Successf("applied at %s", applyResp.GetAppliedAt())
	if h := applyResp.GetContentHash(); h != "" {
		cliout.Field(os.Stdout, "content", h[:12])
	}
	if cfg.OutputDir != "" {
		cliout.Infof("regenerate the typed client under %s with `tide generate`", cfg.OutputDir)
	}
	return 0
}

// waitForDecision retries the apply until it is decided or the budget runs out.
//
// Retrying the apply rather than polling a status RPC, for two reasons. It is
// the apply that has to succeed, so retrying it tests the thing that matters
// rather than a proxy for it; and each attempt is already idempotent — the
// server upsert refuses to touch a row that has been approved or rejected, so a
// retry cannot undo the decision it is waiting for.
//
// A rejection ends the wait immediately. Sitting in a retry loop against a
// decision that will never change is how a pipeline burns thirty minutes to
// report something it knew at the first attempt.
func waitForDecision(ctx context.Context, client *adminClient, req *adminpb.ApplyMigrationRequest,
	budget, rpcTimeout time.Duration, first error,
) (*adminpb.ApplyMigrationResponse, error) {
	if strings.Contains(status.Convert(first).Message(), "was rejected by") {
		return nil, first
	}
	cliout.Infof("waiting up to %s for a decision — %s", budget, status.Convert(first).Message())

	const poll = 15 * time.Second
	deadline := time.Now().Add(budget)
	last := first
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, last
		case <-time.After(poll):
		}
		// Per attempt, not per wait. The outer context now spans the whole
		// approval budget, so without this a server that accepts the
		// connection and never answers would consume the entire half hour in
		// one call and report the original refusal at the end of it.
		resp, err := func() (*adminpb.ApplyMigrationResponse, error) {
			attemptCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
			defer cancel()
			return client.ApplyMigration(attemptCtx, req)
		}()
		if err == nil {
			return resp, nil
		}
		last = err
		if status.Code(err) != codes.FailedPrecondition {
			return nil, err
		}
		if strings.Contains(status.Convert(err).Message(), "was rejected by") {
			return nil, err
		}
	}
	return nil, last
}

func printImpactReport(p *adminpb.PlanSchemaResponse) {
	if len(p.ImpactReport) > 0 {
		cliout.Header(os.Stdout, "impact")
		for _, e := range p.ImpactReport {
			if e.Affected {
				cliout.Row(os.Stdout, "warn", cliout.Bold(e.Caller), e.Detail)
			} else {
				cliout.Row(os.Stdout, "muted", cliout.Faint(e.Caller), e.Detail)
			}
		}
		fmt.Println()
	}
	if len(p.Extensions) > 0 {
		printExtensions(p.Extensions)
		fmt.Println()
	}
	printIndexDrift(p)
	printCheckDrift(p)
	printColumnDrift(p)
}

// printColumnDrift surfaces columns whose live type/width differs from the
// declaration. `tide apply` refuses on them unless ATLANTIS_ALLOW_COLUMN_DRIFT=1.
func printColumnDrift(p *adminpb.PlanSchemaResponse) {
	if p.ColumnDriftError != "" {
		cliout.Header(os.Stdout, "column drift")
		cliout.Row(os.Stdout, "warn", "check skipped", p.ColumnDriftError)
		fmt.Println()
		return
	}
	if len(p.ColumnDrift) == 0 && len(p.ColumnDriftNotes) == 0 {
		return
	}
	cliout.Header(os.Stdout, "column drift")
	for _, d := range p.ColumnDrift {
		cliout.Row(os.Stdout, "coral", d.Schema+"."+d.Table+"."+d.Column, "declared "+d.Declared+", live "+d.Live)
	}
	for _, n := range p.ColumnDriftNotes {
		cliout.Row(os.Stdout, "muted", "note", n)
	}
	cliout.SubRow(os.Stdout, "reconcile out-of-band, or ATLANTIS_ALLOW_COLUMN_DRIFT=1 to apply anyway")
	fmt.Println()
}

// printCheckDrift surfaces CHECK constraints that diverge between the .atl
// and the live table. Like index drift, these don't change the plan class,
// but `tide apply` refuses on them unless ATLANTIS_ALLOW_CHECK_DRIFT=1.
func printCheckDrift(p *adminpb.PlanSchemaResponse) {
	if p.CheckDriftError != "" {
		cliout.Header(os.Stdout, "check drift")
		cliout.Row(os.Stdout, "warn", "check skipped", p.CheckDriftError)
		fmt.Println()
		return
	}
	if len(p.CheckDrift) == 0 && len(p.CheckDriftNotes) == 0 {
		return
	}
	cliout.Header(os.Stdout, "check drift")
	for _, d := range p.CheckDrift {
		if d.GetKind() == adminpb.CheckDriftKind_CHECK_DRIFT_KIND_LIVE_NOT_DECLARED {
			cliout.Row(os.Stdout, "coral", d.Schema+"."+d.Table, "undeclared CHECK "+d.ConstraintName)
			cliout.SubRow(os.Stdout, "live: "+d.Definition)
		} else {
			cliout.Row(os.Stdout, "coral", d.Schema+"."+d.Table, "declared CHECK not enforced live")
			cliout.SubRow(os.Stdout, "declared: "+d.Declared)
		}
	}
	for _, n := range p.CheckDriftNotes {
		cliout.Row(os.Stdout, "muted", "note", n)
	}
	cliout.SubRow(os.Stdout, "reconcile out-of-band, or ATLANTIS_ALLOW_CHECK_DRIFT=1 to apply anyway")
	fmt.Println()
}

// printIndexDrift surfaces live UNIQUE indexes the schema doesn't declare.
// These don't change the plan class, but `tide apply` will refuse on them
// unless ATLANTIS_ALLOW_INDEX_DRIFT=1 — so the warning is the operator's
// heads-up before they commit to applying.
func printIndexDrift(p *adminpb.PlanSchemaResponse) {
	if p.IndexDriftError != "" {
		cliout.Header(os.Stdout, "index drift")
		cliout.Row(os.Stdout, "warn", "check skipped", p.IndexDriftError)
		fmt.Println()
		return
	}
	if len(p.IndexDrift) == 0 && len(p.IndexDriftNotes) == 0 {
		return
	}
	cliout.Header(os.Stdout, "index drift")
	for _, d := range p.IndexDrift {
		desc := "(" + strings.Join(d.Columns, ", ") + ")"
		if d.Partial {
			desc += " WHERE " + d.Predicate
		}
		cliout.Row(os.Stdout, "coral", d.Schema+"."+d.IndexName, "undeclared UNIQUE on "+desc)
		cliout.SubRow(os.Stdout, fmt.Sprintf(`DROP INDEX "%s"."%s";  (or ATLANTIS_ALLOW_INDEX_DRIFT=1 to apply anyway)`, d.Schema, d.IndexName))
	}
	for _, n := range p.IndexDriftNotes {
		cliout.Row(os.Stdout, "muted", "note", n)
	}
	fmt.Println()
}

// collectPCFiles walks every schema path and reads every .atl file. Paths
// are stored relative to the caller's repo root so the server's error
// messages are useful in the caller's context.
func collectPCFiles(paths []string) ([]*adminpb.SubmittedFile, error) {
	var out []*adminpb.SubmittedFile
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Hidden directories are not schema, and one of them is ours.
			//
			// `tide pull` writes the organisation's merged schema into
			// .tide-cache/schema/, and `tide plan` pulls before it plans. Under
			// the documented `schema_paths: ["."]` the next plan submits this
			// caller's entities twice, from source and from the mirror, and the
			// server refuses with "duplicate entity" naming a path nothing in
			// the repository wrote. The first apply in a fresh workspace
			// succeeds and every one after it fails.
			//
			// Every dot-directory is skipped, not .tide-cache alone, which also
			// leaves out .git and vendored virtualenvs.
			//
			// The root itself is never skipped: `schema_paths: ["."]` arrives
			// here as a path of exactly ".".
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".atl" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, &adminpb.SubmittedFile{Path: path, Content: data})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", root, err)
		}
	}
	return out, nil
}
