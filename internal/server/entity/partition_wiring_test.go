package entity

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every statement this package issues must be tenant-scoped by construction.
//
// This is a source-level test because the thing it guards is an absence, and an
// absence has no behaviour to observe. An earlier version of this wiring was
// reviewed, tested and merged-ready while four of six targeted mutations
// survived — including deleting the scope from the read path and every bind
// from the write paths. Nothing failed, because every test in the package
// exercised entities that declare no `partition by`, and for those the scoped
// path and the unscoped path are the same path.
//
// So the assertion is structural instead: a read may not reach the pool
// directly, and a transaction may not be opened without binding. Adding a
// handler that forgets either is then a compile-adjacent failure rather than a
// discovery somebody makes after a cross-tenant read.
//
// partition.go is the one exemption, because it is where the scoping lives.

// poolExempt lists files allowed to touch s.pool directly, with the reason.
var poolExempt = map[string]string{
	"partition.go": "defines the scoping; its pool access IS the mechanism",
}

// bindNames are the calls that count as binding a transaction.
var bindNames = map[string]bool{
	"bindWrite":     true,
	"BindPartition": true,
}

func parseEntitySources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("scanned no source files; this test would pass without checking anything")
	}
	return fset, out
}

// poolCall reports whether n calls sel on something that names the pool.
//
// It matches on the FIELD name, not on a receiver spelling. The first version
// compared against the literal string "s.pool", which a review defeated three
// ways in a minute: renaming the receiver to `srv`, assigning `p := s.pool` and
// calling `p.QueryRow`, and taking the method value `queryRow := s.pool.QueryRow`.
// None of those change what the code does; all three made the guard blind.
//
// So: any `<anything>.pool.<sel>(...)`, plus any call on a local that was
// assigned from something ending in `.pool`. That is still a syntactic check
// and still defeatable by enough indirection — which is why the handler tests
// in partition_handler_test.go, which assert on what reaches the database, are
// the primary guard and this is the cheap backstop.
func poolCall(n ast.Node, aliases map[string]bool, sel string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	fun, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fun.Sel.Name != sel {
		return false
	}
	switch recv := fun.X.(type) {
	case *ast.SelectorExpr:
		// <receiver>.pool.<sel>(...), whatever the receiver is called.
		return recv.Sel.Name == "pool"
	case *ast.Ident:
		// A local assigned from something ending in .pool.
		return aliases[recv.Name]
	}
	return false
}

// poolAliases collects locals assigned from a `.pool` selector, so
// `p := s.pool` does not hide the calls that follow.
func poolAliases(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			sel, ok := rhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "pool" || i >= len(assign.Lhs) {
				continue
			}
			if id, ok := assign.Lhs[i].(*ast.Ident); ok {
				out[id.Name] = true
			}
		}
		return true
	})
	return out
}

func TestNoReadReachesThePoolDirectly(t *testing.T) {
	fset, files := parseEntitySources(t)

	for name, file := range files {
		if reason, ok := poolExempt[name]; ok {
			t.Logf("skipping %s: %s", name, reason)
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			aliases := poolAliases(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				for _, method := range []string{"Query", "QueryRow", "Exec"} {
					if !poolCall(n, aliases, method) {
						continue
					}
					t.Errorf("%s: %s calls pool.%s directly. A read on the bare pool "+
						"has no transaction, so a partitioned entity runs it with no "+
						"tenant bound — returning nothing on a role row-level security "+
						"applies to, and every tenant's rows on a role that bypasses "+
						"it. Route it through scopedRead or scopedReadIf",
						fset.Position(n.Pos()), fn.Name.Name, method)
				}
				return true
			})
		}
	}
}

func TestEveryTransactionBindsTheTenant(t *testing.T) {
	fset, files := parseEntitySources(t)

	for name, file := range files {
		if _, ok := poolExempt[name]; ok {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			aliases := poolAliases(fn)
			var beginPos token.Pos
			var bindPos token.Pos
			var binds bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if poolCall(n, aliases, "BeginTx") && !beginPos.IsValid() {
					beginPos = n.Pos()
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if bindNames[f.Name] && !bindPos.IsValid() {
						binds, bindPos = true, n.Pos()
					}
				case *ast.SelectorExpr:
					if bindNames[f.Sel.Name] && !bindPos.IsValid() {
						binds, bindPos = true, n.Pos()
					}
				}
				return true
			})

			// The bind must come BEFORE anything else in the transaction, and
			// position is what says so. Checking only that a bind exists
			// somewhere in the function let a review move it after every step of
			// a procedure with the whole package green.
			if beginPos.IsValid() && binds && bindPos < beginPos {
				t.Errorf("%s: %s binds the tenant before it opens the transaction, "+
					"so the bind applies to nothing", fset.Position(bindPos), fn.Name.Name)
			}
			if beginPos.IsValid() && !binds {
				t.Errorf("%s: %s opens a transaction with s.pool.BeginTx and never "+
					"binds the caller's tenant. Every statement in it runs unscoped, "+
					"so a partitioned entity is written and read without the policy "+
					"having anything to compare against. Call s.bindWrite (or "+
					"runtime.BindPartition) before the first statement",
					fset.Position(beginPos), fn.Name.Name)
			}
		}
	}
}

// The exemption list must not quietly grow.
//
// Adding a file to poolExempt is how this test gets defeated without anybody
// noticing — the diff reads as one line of config. Pinning the contents makes
// widening it a deliberate edit to an assertion that says why.
func TestPoolExemptionsAreOnlyTheScopingItself(t *testing.T) {
	if len(poolExempt) != 1 {
		t.Errorf("poolExempt has %d entries, want exactly 1. Every addition is a "+
			"file allowed to issue unscoped statements; if one is genuinely "+
			"needed, say here why it cannot go through scopedRead", len(poolExempt))
	}
	if _, ok := poolExempt["partition.go"]; !ok {
		t.Error("partition.go is no longer exempt, so either the scoping moved " +
			"or the exemption was repurposed")
	}
}
