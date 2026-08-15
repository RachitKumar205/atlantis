package admin

import (
	"go/ast"
	"testing"
)

// TestInspectSchemaOpensAReadOnlyTransaction pins the guarantee that makes
// `tide inspect` safe to point at production.
//
// This is an AST test because what it guards is the ABSENCE of a change, and
// the change is invisible at runtime. Deleting `AccessMode: pgx.ReadOnly`
// breaks no test and alters no output: inspect still returns the same drift
// report, because it does not currently write. It only matters later, when
// somebody extends the path it shares with adopt — and by then the thing that
// would have caught them is gone.
//
// Mutation-checked: with the access mode present, a probe UPDATE inside
// InspectSchema fails at Postgres with SQLSTATE 25006. Without it, the same
// probe succeeds and only a commit makes it visible. So this assertion is the
// difference between "cannot" and "does not happen to".
func TestInspectSchemaOpensAReadOnlyTransaction(t *testing.T) {
	_, files := parsePackageSources(t)

	var found, readOnly bool
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || fn.Name.Name != "InspectSchema" {
				continue
			}
			if receiverTypeName(fn) != "Service" {
				continue
			}
			found = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				// pgx.TxOptions{AccessMode: pgx.ReadOnly}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "TxOptions" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					k, ok := kv.Key.(*ast.Ident)
					if !ok || k.Name != "AccessMode" {
						continue
					}
					if v, ok := kv.Value.(*ast.SelectorExpr); ok && v.Sel.Name == "ReadOnly" {
						readOnly = true
					}
				}
				return true
			})
		}
	}

	if !found {
		t.Fatal("InspectSchema is not declared on *Service — this guard is stale")
	}
	if !readOnly {
		t.Error("InspectSchema does not open its transaction with " +
			"pgx.TxOptions{AccessMode: pgx.ReadOnly}.\n" +
			"  That access mode is the only thing making 'inspect writes nothing' a " +
			"property Postgres enforces rather than one a reviewer has to notice. It " +
			"shares compareToLive with AdoptBaseline, whose next line is " +
			"persistCheckpoint.")
	}
}

// writeHelpers are the functions that put something in the database on the
// adopt path. InspectSchema shares everything up to them and must call none.
var writeHelpers = []string{"persistCheckpoint", "upsertCallerFiles", "insertAdoptHistory"}

// TestInspectSchemaCallsNoWriteHelper is the cheaper, blunter half.
//
// The access-mode test above is the real guard. This one catches the case that
// would otherwise be a confusing runtime failure rather than a review comment:
// somebody adds a write on this path, and it fails in production against a
// read-only transaction instead of failing here.
func TestInspectSchemaCallsNoWriteHelper(t *testing.T) {
	_, files := parsePackageSources(t)

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || fn.Name.Name != "InspectSchema" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if contains(writeHelpers, sel.Sel.Name) {
					t.Errorf("InspectSchema calls %s. Inspect answers a question; it "+
						"does not record an answer.", sel.Sel.Name)
				}
				return true
			})
		}
	}
}
