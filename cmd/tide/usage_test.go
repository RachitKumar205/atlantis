package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestEverySubcommandAppearsInUsage stops a command existing that nobody can
// find.
//
// tide's dispatch is a switch and its usage is a hand-written list, so the two
// can disagree silently — and did: `inspect` was dispatched and working while
// `tide` with no arguments never mentioned it. A user's only route to a
// subcommand they have not been told about is that list.
//
// tidectl does not have this problem because its commands are a slice a test
// can read. tide's switch is load-bearing in a way that resists the same
// treatment (each arm calls a differently-shaped function), so the property is
// asserted here instead of restructured.
func TestEverySubcommandAppearsInUsage(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var cases []string
	var usageText strings.Builder
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		switch fn.Name.Name {
		case "main":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							cases = append(cases, v)
						}
					}
				}
				return true
			})
		case "printUsage":
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, err := strconv.Unquote(lit.Value); err == nil {
						usageText.WriteString(v)
						usageText.WriteString("\n")
					}
				}
				return true
			})
		}
	}

	if len(cases) < 10 {
		t.Fatalf("found %d subcommands in main's switch; this test is not reading "+
			"what it thinks it is", len(cases))
	}
	if usageText.Len() == 0 {
		t.Fatal("printUsage produced no string literals; this test is not reading it")
	}

	usage := usageText.String()
	for _, name := range cases {
		if !strings.Contains(usage, "tide "+name) {
			t.Errorf("subcommand %q is dispatched but never named in printUsage. "+
				"A user with no prior knowledge of it cannot discover it.", name)
		}
	}
}
