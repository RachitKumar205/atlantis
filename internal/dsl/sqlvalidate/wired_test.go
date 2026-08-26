package sqlvalidate_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryValidatorHasAProductionCaller fails when an exported Validate*
// function in this package has no call site outside it.
//
// A validator with no caller is either dead code or a control that is not
// running. ValidateBackfillExpression was the second: tested, documented in
// three comments as running at admin.PlanSchema time, and never called, while
// the expression it checks was interpolated into `SET <col> = <expr>` against
// the live table.
//
// It looks for the name in call position, not for a request path that reaches
// it. Leaving admin.validateBackfillExpressions in the tree while deleting the
// one call to it from BeginBackfillPlan keeps this test green; the per-RPC
// assertion in internal/server/admin covers that case.
func TestEveryValidatorHasAProductionCaller(t *testing.T) {
	root := repoRoot(t)

	exported := exportedValidators(t, filepath.Join(root, "internal", "dsl", "sqlvalidate"))
	if len(exported) == 0 {
		t.Fatal("found no exported Validate* functions; this test is not looking where it thinks")
	}

	called := calledIdentifiers(t, root)
	for _, name := range exported {
		if !called[name] {
			t.Errorf("sqlvalidate.%s has no non-test caller anywhere in the repo.\n"+
				"Either it is dead code and should be deleted, or it is a validation "+
				"that is silently not running. It was the latter for %s until 2026-07-30.",
				name, name)
		}
	}
}

// exportedValidators lists exported functions named Validate* declared in dir.
func exportedValidators(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "Validate") {
				out = append(out, fn.Name.Name)
			}
		}
	}
	return out
}

// calledIdentifiers walks every non-test .go file under root and returns the
// set of function names that appear in call position.
//
// Matching on the selector's final identifier rather than resolving the package
// means a same-named method elsewhere would count as a caller. That is a
// tolerable false negative for a guard whose failure mode is "someone deleted
// the only call site" — a name collision hides the alarm, but nothing about
// adding a collision is a plausible way to remove a validator by accident.
func calledIdentifiers(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()

	skip := map[string]bool{".git": true, "node_modules": true, "gen": true, "pb": true}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable paths are not evidence of anything
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The declaring package does not count as its own caller.
		if strings.Contains(filepath.Dir(path), filepath.Join("dsl", "sqlvalidate")) {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // generated or build-tagged files that don't parse standalone
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				out[fn.Sel.Name] = true
			case *ast.Ident:
				out[fn.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate repo root (no go.mod within 6 parents)")
	return ""
}
