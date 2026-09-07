package cliflag

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryFlagSetParsesThroughThisPackage stops a subcommand parsing with
// flag.FlagSet.Parse, which drops every flag after the first positional
// argument.
//
// The rule is over the AST: a FlagSet is always bound to an identifier named
// fs, and fs.Parse is never called. Either half alone can be evaded; a
// FlagSet under another name is reported by the first check.
func TestEveryFlagSetParsesThroughThisPackage(t *testing.T) {
	for _, dir := range []string{"../../cmd/tide", "../../cmd/tidectl"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var sawFlagSet bool
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for i, rhs := range n.Rhs {
						if !makesFlagSet(rhs) {
							continue
						}
						sawFlagSet = true
						if id, ok := n.Lhs[i].(*ast.Ident); !ok || id.Name != "fs" {
							t.Errorf("%s: a FlagSet must be named fs so this check sees its Parse calls", fset.Position(n.Pos()))
						}
					}
				case *ast.ValueSpec:
					for i, v := range n.Values {
						if !makesFlagSet(v) {
							continue
						}
						sawFlagSet = true
						if i >= len(n.Names) || n.Names[i].Name != "fs" {
							t.Errorf("%s: a FlagSet must be named fs so this check sees its Parse calls", fset.Position(n.Pos()))
						}
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Parse" {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "fs" {
						t.Errorf("%s: fs.Parse stops at the first positional argument; use cliflag.Parse or cliflag.ParseNoArgs", fset.Position(n.Pos()))
					}
				}
				return true
			})
		}
		if !sawFlagSet {
			t.Errorf("%s: no FlagSet construction found; the check is scanning nothing", dir)
		}
	}
}

// makesFlagSet reports whether expr is flag.NewFlagSet(...) or the tidectl
// flagSet(...) helper around it.
func makesFlagSet(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		return ok && pkg.Name == "flag" && fn.Sel.Name == "NewFlagSet"
	case *ast.Ident:
		return fn.Name == "flagSet"
	}
	return false
}
