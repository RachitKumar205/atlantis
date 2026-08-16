package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// TestCommandBudgetLetsTheApprovalWaitActuallyWait pins the arithmetic that
// made --wait-for-approval inert.
//
// The command ran under one context deadline set from --timeout, default 30s,
// and that same context bounded the approval retry loop. So
// `tide apply --wait-for-approval=30m` waited thirty SECONDS and then reported
// the refusal it had already seen. Nothing logged a timeout; the pipeline just
// gave up early, which reads like a slow reviewer rather than a broken flag.
func TestCommandBudgetLetsTheApprovalWaitActuallyWait(t *testing.T) {
	const rpc = 30 * time.Second

	// No wait: the budget is exactly one RPC. Widening it here would hold a CI
	// runner open for no reason, which is why waiting is opt-in.
	if got := commandBudget(rpc, 0); got != rpc {
		t.Errorf("with no wait, budget = %s, want %s", got, rpc)
	}
	if got := commandBudget(rpc, -time.Minute); got != rpc {
		t.Errorf("with a negative wait, budget = %s, want %s", got, rpc)
	}

	// With a wait, the budget must EXCEED it — a budget equal to the wait
	// leaves no room for the attempt that discovers the approval.
	for _, wait := range []time.Duration{time.Minute, 30 * time.Minute, 4 * time.Hour} {
		got := commandBudget(rpc, wait)
		if got <= wait {
			t.Errorf("wait=%s gives budget %s, which is not longer than the wait — "+
				"the loop is cut off before its last attempt can finish", wait, got)
		}
		if got != wait+rpc {
			t.Errorf("wait=%s gives budget %s, want %s", wait, got, wait+rpc)
		}
	}

	// The specific case from the report.
	if got := commandBudget(30*time.Second, 30*time.Minute); got < 30*time.Minute {
		t.Errorf("--timeout=30s --wait-for-approval=30m gives a %s budget; the "+
			"flag cannot wait 30 minutes inside it", got)
	}
}

// TestBackfillArmHonoursDryRun is structural, and deliberately narrow: it
// checks that the BACKFILL_REQUIRED case consults dryRun before it reaches
// doBeginBackfill.
//
// Driving cmdApply needs a live server, and the property is about ordering
// inside one switch arm — the same shape as the guard that was missing. Every
// other arm checked the flag; this one went straight to doBeginBackfill, so
// `tide apply --dry-run --backfill` STARTED the backfill. The flag whose whole
// promise is "do not apply" performed the longest-running write in the
// product.
//
// This walks the actual case clause rather than grepping the function, so a
// dryRun check that moved into a different arm would not satisfy it.
func TestBackfillArmHonoursDryRun(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "apply.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse apply.go: %v", err)
	}

	var arm *ast.CaseClause
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if sel, ok := e.(*ast.SelectorExpr); ok &&
				sel.Sel.Name == "PlanClass_PLAN_CLASS_BACKFILL_REQUIRED" {
				arm = cc
			}
		}
		return true
	})
	if arm == nil {
		t.Fatal("no BACKFILL_REQUIRED case clause in apply.go — this test is " +
			"checking nothing")
	}

	// Find the dryRun reference and the doBeginBackfill call, and require the
	// first to come before the second. Position order inside one clause is
	// exactly the ordering that was wrong.
	var dryRunPos, beginPos token.Pos
	ast.Inspect(arm, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "dryRun" && !dryRunPos.IsValid() {
			dryRunPos = id.Pos()
		}
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "doBeginBackfill" {
				beginPos = call.Pos()
			}
		}
		return true
	})

	if !dryRunPos.IsValid() {
		t.Fatal("the BACKFILL_REQUIRED arm never mentions dryRun, so " +
			"`tide apply --dry-run --backfill` starts the backfill — the one " +
			"flag that promises not to act performing the longest write there is")
	}
	if !beginPos.IsValid() {
		t.Fatal("the BACKFILL_REQUIRED arm no longer calls doBeginBackfill; this " +
			"test is pinned to a shape that has moved")
	}
	if dryRunPos > beginPos {
		t.Error("the BACKFILL_REQUIRED arm calls doBeginBackfill before it checks " +
			"dryRun, so the backfill starts and the flag is read too late")
	}
}
