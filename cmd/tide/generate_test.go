package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	iofs "io/fs"
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
		// A leading digit. The first version of this check accepted it, because
		// it tested only "is every rune alphanumeric or underscore" and never
		// "is the FIRST rune a letter". `2024_events` is an ordinary legacy
		// table name; it yields "2024Events", which lexes as a number followed
		// by an identifier, so the .atl written for it did not parse and the
		// customer got a syntax error in a file they had not written.
		"2024Events",
		"1",
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
		"_private":   "_private.atl",
		// Non-ASCII letters. The DSL's isIdentStart is unicode.IsLetter, so
		// this is a valid entity name and generated fine before any check
		// existed. An ASCII-only check refused it and told the customer to
		// rename a table that was never the problem.
		"CaféOrders": "caféorders.atl",
		"Ünicode":    "ünicode.atl",
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

// TestRefuseMissingSchema drives the decision directly.
//
// The first version of this test was an AST tripwire: it walked cmdInspect
// collecting *ast.Ident and *ast.BasicLit nodes and asserted the strings
// "generating" and "ErrNotExist" appeared somewhere in the result. A review
// showed it could not catch the defect it existed for — flipping either
// negation (`!generating` to `generating`) restores the original bug verbatim
// while leaving every identifier untouched, because a negation is a UnaryExpr
// the walk never visited. It reproduced that: both assertions still passed
// against a body with both negations inverted.
//
// So the decision moved into refuseMissingSchema and this drives it across the
// whole input space. Each row is a case a user actually hits.
func TestRefuseMissingSchema(t *testing.T) {
	other := errors.New("permission denied")

	for _, tc := range []struct {
		name       string
		generating bool
		walkErr    error
		files      int
		wantRefuse bool
		wantErr    error
	}{
		// The case the whole fix exists for: a fresh repo, no schema directory.
		{"generate, no schema dir", true, iofs.ErrNotExist, 0, false, nil},
		// A schema directory that exists and is empty.
		{"generate, empty schema dir", true, nil, 0, false, nil},
		// Generation in a repo that already has declarations is ordinary.
		{"generate, files present", true, nil, 3, false, nil},

		// Plain inspect has nothing to compare against and must still refuse,
		// which is the half a looser fix would have broken.
		{"inspect, no schema dir", false, iofs.ErrNotExist, 0, true, iofs.ErrNotExist},
		{"inspect, empty schema dir", false, nil, 0, true, nil},
		{"inspect, files present", false, nil, 3, false, nil},

		// A permission error is not a fresh repo, on either path.
		{"generate, unreadable dir", true, other, 0, true, other},
		{"inspect, unreadable dir", false, other, 0, true, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuse, why := refuseMissingSchema(tc.generating, tc.walkErr, tc.files)
			if refuse != tc.wantRefuse {
				t.Errorf("refuse = %v, want %v", refuse, tc.wantRefuse)
			}
			if tc.wantErr == nil && why != nil {
				t.Errorf("returned error %v, want none", why)
			}
			if tc.wantErr != nil && !errors.Is(why, tc.wantErr) {
				t.Errorf("returned error %v, want %v", why, tc.wantErr)
			}
		})
	}
}

// TestCmdInspectUsesTheSharedDecision keeps the decision from being reinlined.
// refuseMissingSchema is only worth having if cmdInspect actually calls it;
// a copy of the logic back inside cmdInspect would leave the table above
// passing while the command did something else.
func TestCmdInspectUsesTheSharedDecision(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "inspect.go", nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse inspect.go: %v", err)
	}
	var calls int
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdInspect" {
			return true
		}
		ast.Inspect(fn, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "refuseMissingSchema" {
				calls++
			}
			return true
		})
		return false
	})
	if calls != 1 {
		t.Errorf("cmdInspect calls refuseMissingSchema %d times, want exactly 1 — "+
			"the tested decision is not the one the command makes", calls)
	}
}
