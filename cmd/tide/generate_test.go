package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// generatedFileName turns a name the SERVER chose into a path this process
// writes to. Everything it guards against arrives over the wire.

func TestGeneratedFileNameRefusesPathTraversal(t *testing.T) {
	// A Postgres identifier is only constrained by what fits inside quotes, so
	// every one of these is a table a customer could really have, and
	// SuggestedName carries the characters through. filepath.Join CLEANS its
	// result, so ".." is not neutralised by joining — it resolves.
	for _, name := range []string{
		"../../etc/passwd",
		"..",
		"a/b",
		`a\b`,
		"/absolute",
		"with space",
		"semi;colon",
		"dot.dot",
		"",
	} {
		if got, err := generatedFileName(name); err == nil {
			t.Errorf("entity name %q was accepted as filename %q — the server "+
				"chooses this name from a table name, so accepting it lets a "+
				"table name decide where this process writes", name, got)
		}
	}
}

func TestGeneratedFileNameAcceptsOrdinaryNames(t *testing.T) {
	for name, want := range map[string]string{
		"Customer":   "customer.atl",
		"OrderLine":  "orderline.atl",
		"order_line": "order_line.atl",
		"V2":         "v2.atl",
	} {
		got, err := generatedFileName(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %q, want %q", name, got, want)
		}
	}
}

// TestGenerateToleratesARepoWithNoSchemaYet is structural rather than
// behavioural, because driving cmdInspect needs a live server and the property
// is about which guard runs before which.
//
// `tide inspect --generate` exists to write the first .atl files in a repo. It
// refused to run in exactly that repo: the `len(files) == 0` check sat above
// the `--generate` dispatch and returned 3 without ever dialling. Confirmed
// twice by review and reproduced with a built binary.
//
// This reads cmdInspect and requires the empty-files refusal to be conditional
// on not generating. It is a tripwire, not a proof — but the thing that went
// wrong was an ordering, and an ordering is what it checks.
func TestGenerateToleratesARepoWithNoSchemaYet(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "inspect.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse inspect.go: %v", err)
	}

	var body string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdInspect" {
			return true
		}
		var sb strings.Builder
		ast.Inspect(fn, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok {
				sb.WriteString(id.Name + " ")
			}
			if lit, ok := m.(*ast.BasicLit); ok {
				sb.WriteString(lit.Value + " ")
			}
			return true
		})
		body = sb.String()
		return false
	})
	if body == "" {
		t.Fatal("cmdInspect not found in inspect.go — this test is checking nothing")
	}

	if !strings.Contains(body, "generating") {
		t.Error("cmdInspect no longer computes a `generating` flag, so the " +
			"empty-schema refusal is unconditional again and " +
			"`tide inspect --generate` cannot run in a repo with no .atl files")
	}
	if !strings.Contains(body, "ErrNotExist") {
		t.Error("cmdInspect no longer tolerates a missing schema directory; " +
			"`tide inspect --generate` fails before dialling in a fresh repo")
	}
}
