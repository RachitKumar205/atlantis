package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The server must actually register its built-in jobs.
//
// jobs.Builtins() lists them and jobs/builtins_test.go covers that list being
// complete. A complete list nothing calls is still no registration:
// RegisterSweeper's doc comment named cmd/server/main.go as its caller while no
// such call existed. Both are pinned.
//
// Read from source because there is nothing else to read. Whether main()
// registered its jobs is not observable without booting a server against a
// database, and a boot test would not fail when the call is deleted — it would
// simply schedule nothing, which is the silence this is guarding against.
func TestServerRegistersBuiltinJobs(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Only inside run(), and only on a branch that can execute. A bare search
	// of the file passes when the call sits in dead code or in a function
	// nothing invokes, which is indistinguishable from the bug: present in the
	// source, absent at runtime.
	var runBody *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			runBody = fn.Body
		}
	}
	if runBody == nil {
		t.Fatal("cmd/server has no run() function, so this test cannot tell " +
			"whether the built-in jobs are registered on a reachable path")
	}

	var called, scheduled bool
	var walk func(ast.Node) bool
	walk = func(n ast.Node) bool {
		// Do not descend into a branch that provably never runs.
		if ifs, ok := n.(*ast.IfStmt); ok {
			if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "false" {
				if ifs.Else != nil {
					ast.Inspect(ifs.Else, walk)
				}
				return false
			}
		}
		// A jobs.Scheduler value being constructed on a live path. Registering
		// built-ins without running a scheduler leaves the schedules in the
		// table and nothing evaluating them, which is the state this whole
		// component was written to end.
		if lit, ok := n.(*ast.CompositeLit); ok {
			if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Scheduler" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "jobs" {
					scheduled = true
				}
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RegisterBuiltins" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "jobs" {
			called = true
		}
		return true
	}
	ast.Inspect(runBody, walk)

	if !called {
		t.Error("cmd/server's run() never reaches a jobs.RegisterBuiltins call, so no built-in job " +
			"is registered or scheduled. Nothing fails, nothing logs: the reaper " +
			"simply never runs and parked objects accumulate forever. This is the " +
			"exact shape of the bug that left the TTL sweeper dead in production")
	}

	if !scheduled {
		t.Error("cmd/server's run() never constructs a jobs.Scheduler, so nothing " +
			"evaluates atlantis.job_schedules. The schedule rows exist, the " +
			"handlers are registered, and no job is ever enqueued — which is " +
			"precisely the state the scheduler was written to end")
	}
}
