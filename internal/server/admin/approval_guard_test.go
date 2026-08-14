package admin

import (
	"go/ast"
	"testing"
)

// mustGate names the RPCs that execute schema DDL derived from caller-submitted
// files, and therefore have to consult the change policy.
//
// RollbackSchema is deliberately absent, and the omission is a decision rather
// than an oversight. It returns the schema to a state that already passed this
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
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gateOnChangePolicy" {
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
			t.Errorf("%s executes schema DDL but never calls gateOnChangePolicy. "+
				"Whatever the change policy says, this entry point ignores it — and a "+
				"gate one RPC honours and another does not is a suggestion about which "+
				"RPC to call.", name)
		}
	}
}
