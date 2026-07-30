package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowedHandlerGuards are the two checks that legitimately remain in method
// bodies, and the reason each cannot move to the interceptor.
//
// An interceptor sees the method name and the connection. It does not see the
// request body, so it cannot tell that req.Caller names a different caller than
// the certificate did — bindCallerIdentity is what keeps SCHEMA_APPLY meaning
// "may write to mine" rather than "may write to anyone's". And the transport
// restriction is not a statement about the caller at all: an edge-terminated
// connection is held to a different standard regardless of what its identity
// was granted.
var allowedHandlerGuards = map[string]bool{
	"bindCallerIdentity":     true,
	"guardOperatorTransport": true,
	"forwardedDenied":        true, // the shared implementation of the two above
}

// TestNoHandlerRollsItsOwnAuthorization fails when a method on Service calls
// something that looks like a hand-written authorization check.
//
// The bug this package spent a phase removing was not any particular gate being
// wrong. It was that authorization lived in method bodies, so whether an RPC
// was protected depended on whether its author remembered to protect it — and
// one forgotten call site was indistinguishable from a deliberate public
// endpoint. Capabilities are declared in admin.proto and enforced by an
// interceptor precisely so that omission is impossible. A new authorize* helper
// in a handler would quietly reopen that door for whatever methods happened to
// call it.
//
// This matches on naming, so `gateOperator` or `ensureCaller` would slip past
// it. That is a real limit and the reason it is one of two tests here rather
// than the whole guard: the other asserts positively that the checks which
// must exist are still called.
func TestNoHandlerRollsItsOwnAuthorization(t *testing.T) {
	fset, files := parsePackageSources(t)
	for name, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var called string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				called = fn.Sel.Name
			case *ast.Ident:
				called = fn.Name
			default:
				return true
			}
			if !looksLikeAuthz(called) || allowedHandlerGuards[called] {
				return true
			}
			t.Errorf("%s: %s calls %s. Authorization belongs in admin.proto as a "+
				"required_capability, enforced by internal/server/authz — a check in a "+
				"method body only protects the methods whose authors remembered it. If "+
				"this genuinely cannot move to the interceptor, add it to "+
				"allowedHandlerGuards with the reason.",
				fset.Position(call.Pos()), name, called)
			return true
		})
	}
}

// parsePackageSources parses every non-test .go file in this directory.
//
// Per-file rather than parser.ParseDir: that function is deprecated precisely
// because it ignores build tags, and a guard that silently skipped a
// build-tagged file would be a guard with a hole in it. Fails the test if it
// finds nothing, so neither caller can pass by scanning an empty set.
func parsePackageSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := make(map[string]*ast.File)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = file
	}
	if len(out) == 0 {
		t.Fatal("scanned no source files; the test would pass without checking anything")
	}
	return fset, out
}

// looksLikeAuthz matches the naming this package used before the interceptor,
// plus the obvious variations someone would reach for next.
func looksLikeAuthz(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"authorize", "authz", "checkperm", "requirecap", "requireperm", "mustbe"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// mustBind names the RPCs that take a caller in the request body and write on
// its behalf. Each must call bindCallerIdentity, or CAPABILITY_SCHEMA_APPLY
// stops meaning "may write to my namespace" and starts meaning "may write to
// anyone's" — the interceptor that granted it never saw the body naming the
// target, so nothing else can catch the difference.
var mustBind = []string{"ApplyMigration", "BeginBackfillPlan"}

// TestMutatingRPCsBindTheCaller is the positive half of the guard.
//
// The scan above only fails on something new appearing; deleting the
// bindCallerIdentity line from ApplyMigration would make it pass more easily,
// which is exactly backwards for a privilege escalation dressed as cleanup.
func TestMutatingRPCsBindTheCaller(t *testing.T) {
	_, files := parsePackageSources(t)

	found := map[string]bool{}
	declared := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !contains(mustBind, fn.Name.Name) {
				continue
			}
			declared[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "bindCallerIdentity" {
					found[fn.Name.Name] = true
				}
				return true
			})
		}
	}

	// A renamed RPC would otherwise drop off the list silently, taking its
	// binding requirement with it.
	for _, name := range mustBind {
		if !declared[name] {
			t.Fatalf("mustBind names %s, which this package does not declare — the list is stale", name)
		}
	}

	for _, name := range mustBind {
		if !found[name] {
			t.Errorf("%s does not call bindCallerIdentity. Without it a caller holding "+
				"CAPABILITY_SCHEMA_APPLY can write to any other caller's namespace by "+
				"naming it in the request body — the interceptor cannot see request bodies.",
				name)
		}
	}
}
