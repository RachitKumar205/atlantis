package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The server must check, at boot, whether its database role can bypass
// row-level security.
//
// `partition by` delegates tenant isolation to PostgreSQL. That delegation is
// void if the connecting role is a superuser or holds BYPASSRLS — and the
// failure is silent in the worst way: the policies exist, FORCE is set, the
// catalog agrees, and every read returns every tenant's rows.
// internal/storage/pg executes exactly that against a live database.
//
// Read from source because there is nothing else to read. Whether a boot check
// ran is not observable without booting against a database, and a boot test
// would not fail if the call were deleted — it would simply stop checking,
// which is indistinguishable from checking and finding nothing wrong.
func TestServerChecksTenantIsolationAtBoot(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var runBody *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			runBody = fn.Body
		}
	}
	if runBody == nil {
		t.Fatal("cmd/server has no run() function, so this test cannot tell whether " +
			"the isolation check runs on a reachable path")
	}

	want := map[string]bool{
		"DetectRolePrivileges": false,
		"RequireIsolatedRole":  false,
	}

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
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "pg" {
			if _, tracked := want[sel.Sel.Name]; tracked {
				want[sel.Sel.Name] = true
			}
		}
		return true
	}
	ast.Inspect(runBody, walk)

	if !want["DetectRolePrivileges"] {
		t.Error("cmd/server's run() never reaches pg.DetectRolePrivileges, so nothing " +
			"establishes whether the database role can bypass row-level security. " +
			"Every `partition by` policy would be attached and inert, and no log line " +
			"or error would say so")
	}
	if !want["RequireIsolatedRole"] {
		t.Error("cmd/server's run() reads the role privileges but never calls " +
			"pg.RequireIsolatedRole, so the answer is computed and discarded. " +
			"ATL_REQUIRE_TENANT_ISOLATION would have nothing to gate on")
	}
}
