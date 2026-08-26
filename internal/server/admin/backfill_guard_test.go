package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// BeginBackfillPlan must call the guard and return on its error.
//
// Without it, the guard can be deleted outright with the whole suite green.
//
// This reads source, so it cannot see the arguments: feeding the guard nil
// still passes. Closing that needs the server booted against a real database
// with the refusal asserted, tracked separately. What this covers is that the
// call is present, its condition is a real nil test on the value the call
// returned, the body returns that value, and none of it sits in dead code.
func TestBeginBackfillPlanReachesTheGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "backfill.go", nil, 0)
	if err != nil {
		t.Fatalf("parse backfill.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "BeginBackfillPlan" {
			fn = d
		}
	}
	if fn == nil {
		t.Fatal("BeginBackfillPlan is not in backfill.go; this test now guards nothing")
	}

	// Dead code does not count. A review satisfied the previous version of this
	// test with the real call sitting inside `if false { ... }`.
	dead := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if lit, ok := stmt.Cond.(*ast.Ident); ok && lit.Name == "false" {
			ast.Inspect(stmt.Body, func(m ast.Node) bool { dead[m] = true; return true })
		}
		return true
	})

	obeyed := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Init == nil || dead[n] {
			return true
		}
		assign, ok := stmt.Init.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		bound, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
		if !ok || bound.Name == "_" {
			return true
		}
		calls := false
		ast.Inspect(stmt.Init, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "refuseBackfillOnBlockedTables" {
				calls = true
			}
			return true
		})
		if !calls {
			return true
		}
		// The condition must be exactly `<bound> != nil`. A review satisfied the
		// previous version with `1 != 1`, which is the same class as the
		// `err != nil && false` escape already closed in cmd/server.
		bin, ok := stmt.Cond.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ {
			return true
		}
		lhs, lok := bin.X.(*ast.Ident)
		rhs, rok := bin.Y.(*ast.Ident)
		if !lok || !rok || lhs.Name != bound.Name || rhs.Name != "nil" {
			return true
		}
		// And the body must return the value it just tested. `return nil, nil`
		// satisfied the previous version.
		for _, st := range stmt.Body.List {
			ret, isReturn := st.(*ast.ReturnStmt)
			if !isReturn {
				continue
			}
			for _, res := range ret.Results {
				found := false
				ast.Inspect(res, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && id.Name == bound.Name {
						found = true
					}
					return !found
				})
				if found {
					obeyed = true
				}
			}
		}
		return true
	})

	if !obeyed {
		t.Error("BeginBackfillPlan does not call refuseBackfillOnBlockedTables " +
			"and return its error on a live path. The guard runs and its answer " +
			"is discarded, so a backfill whose every chunk matches zero rows is " +
			"accepted, reports success, and fails later at SET NOT NULL")
	}
}
