package admin

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// mustGate names the RPCs that execute schema DDL derived from caller-submitted
// files, and therefore have to consult the change policy.
//
// RollbackSchema is absent, and the omission is a decision. It returns the
// schema to a state that already passed this
// gate once, it is CAPABILITY_OPERATOR rather than SCHEMA_APPLY, and it is what
// somebody reaches for at 3am. Requiring a second human there is how a gate
// gets switched off permanently — the failure mode is not "rollback was
// approved", it is "the deployment turned approvals off after the first
// incident". See the note in the plan; if that judgement is revisited, this
// list is where it changes.
var mustGate = []string{"ApplyMigration", "BeginBackfillPlan"}

// TestMutatingRPCsGateOnChangePolicy mirrors TestMutatingRPCsBindTheCaller.
//
// The behavioural tests in destructive_gate_test.go prove the gate works on the
// path they drive. This is what stops a SECOND path being added beside it —
// which is exactly how BeginBackfillPlan came to execute destructive DDL with
// no class check at all, while ApplyMigration refused the identical change.
// Nothing failed to compile and no test went red; the two RPCs simply drifted.
func TestMutatingRPCsGateOnChangePolicy(t *testing.T) {
	_, files := parsePackageSources(t)

	found := map[string]bool{}
	declared := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !contains(mustGate, fn.Name.Name) {
				continue
			}
			if receiverTypeName(fn) != "Service" {
				continue
			}
			declared[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// evaluateApplyGates is the composed gate; gateOnChangePolicy
				// is the change-policy layer inside it. The RPCs call the
				// composition — a direct call to the inner layer would skip
				// the apply-policy tier, so it does not satisfy this test.
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "evaluateApplyGates" {
					found[fn.Name.Name] = true
				}
				return true
			})
		}
	}

	// A renamed RPC would otherwise fall off the list silently, taking its gate
	// requirement with it.
	for _, name := range mustGate {
		if !declared[name] {
			t.Fatalf("mustGate names %s, which this package does not declare on *Service "+
				"— the list is stale", name)
		}
		if !found[name] {
			t.Errorf("%s executes schema DDL but never calls evaluateApplyGates. "+
				"Whatever the policies say, this entry point ignores them — and a "+
				"gate one RPC honours and another does not is a suggestion about which "+
				"RPC to call.", name)
		}
	}
}

// gateOutcomes maps every way gateOnChangePolicy can answer to the test that
// proves it answers that way.
//
// The key is the user-visible message, because that is the one thing about an
// arm that is both distinct and stable: line numbers move, and a rename of the
// arm's state constant would not change what an operator reads. If somebody
// edits the wording they are changing what an operator reads and should be
// revisiting the test anyway, which is why an edit failing this test is the
// correct outcome and not an annoyance.
//
// Entries name a test rather than a boolean because "covered" is a claim, and a
// claim with nobody's name on it is the shape this whole file exists to stop.
var gateOutcomes = map[string]string{
	"recorded and waiting for a decision": "TestDirectApplyCannotSkipApproval",
	"the previous request was superseded by a change to the schema it depended on, and has been re-opened for a decision": "TestASupersededPlanReopensRatherThanBricking",
	"the approval expired before it was used and has been re-opened":                                                      "TestAnExpiredApprovalReopens",
	"the approval was for a different version of , so it no longer applies and the request has been re-opened":            "TestADivergedApprovalReopensRatherThanBricking",
	"the previous request expired and has been renewed":                                                                   "TestAPendingPlanThatExpiredIsRenewed",
	"waiting for a decision": "TestDirectApplyCannotSkipApproval",

	"admin: the change policy could not be read, so no change may apply unattended: %v":                                              "TestTheGateFailsClosedWhenThePolicyCannotBeRead",
	"admin: plan class %s cannot be evaluated against the change policy":                                                             "TestEveryClassPresentIsSettable (invariant; the arm is unreachable)",
	"admin: plan %s was rejected by %s%s — change the schema and re-plan; resubmitting the same change does not re-open the request": "TestRejectedPlanDoesNotReopen",
	"admin: plan %s has already been applied":                                                                                        "TestAnAppliedPlanIsNotReapplied",
}

// TestEveryGateOutcomeIsCovered stops an arm being added to the gate with no
// test behind it.
//
// That is not hypothetical. bedca6a added three re-open arms and shipped tests
// for two; deleting recordPendingPlan from the superseded arm left this entire
// package green, and the gap was found by mutation rather than by anything in
// CI. Enumerating the function afterwards turned up four uncovered outcomes,
// not one.
//
// Modelled on cmd/tide/usage_test.go, which requires every subcommand in the
// dispatch switch to appear in the usage text.
func TestEveryGateOutcomeIsCovered(t *testing.T) {
	_, files := parsePackageSources(t)

	var body *ast.BlockStmt
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			if fn.Name.Name == "gateOnChangePolicy" && receiverTypeName(fn) == "Service" {
				body = fn.Body
			}
		}
	}
	if body == nil {
		t.Fatal("this package no longer declares gateOnChangePolicy on *Service, " +
			"so this guard is checking nothing")
	}

	found := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			// approvalRequired(planID, class, role, reason) — the reason is last.
			if fun.Name == "approvalRequired" {
				found[literalText(call.Args[len(call.Args)-1])] = true
			}
		case *ast.SelectorExpr:
			// status.Errorf(code, format, ...) — the format is second.
			// fmt.Errorf is not collected: those wrap an internal failure
			// rather than an answer the gate gives.
			if id, ok := fun.X.(*ast.Ident); ok && id.Name == "status" &&
				fun.Sel.Name == "Errorf" && len(call.Args) >= 2 {
				found[literalText(call.Args[1])] = true
			}
		}
		return true
	})
	delete(found, "")

	// A collector that finds nothing would make the first loop below vacuous —
	// every outcome "covered" because none was seen. This is the assertion that
	// stops that, and it is the direction the equivalent guard in this session
	// was missing when it failed to see a `!`.
	if len(found) == 0 {
		t.Fatal("collected no outcomes from gateOnChangePolicy. The extraction is " +
			"broken, and every assertion below would pass against an empty set")
	}

	for outcome := range found {
		if gateOutcomes[outcome] == "" {
			t.Errorf("gateOnChangePolicy can answer %q and no test is recorded for "+
				"it.\nAdd the test, then add it to gateOutcomes. An arm nobody "+
				"exercises is one a later edit can delete silently — which is how "+
				"the superseded re-open shipped untested.", outcome)
		}
	}
	// The other direction: a stale entry means either the arm was removed or
	// the collector stopped seeing it, and both mean this guard has quietly
	// stopped guarding that outcome.
	var stale []string
	for outcome := range gateOutcomes {
		if !found[outcome] {
			stale = append(stale, outcome)
		}
	}
	sort.Strings(stale)
	for _, outcome := range stale {
		t.Errorf("gateOutcomes lists %q, which gateOnChangePolicy no longer "+
			"produces. Either the arm is gone and the entry should go with it, or "+
			"the extraction above stopped matching and this guard is now blind.",
			outcome)
	}
}

// literalText concatenates the string literals in an expression, ignoring the
// non-literal parts.
//
// `"a " + v + " b"` yields "a  b" — the variable contributes nothing and leaves
// the gap visible, which is stable across changes to what the variable holds.
func literalText(e ast.Expr) string {
	var sb strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				sb.WriteString(s)
			}
		}
		return true
	})
	return sb.String()
}
