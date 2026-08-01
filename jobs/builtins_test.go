package jobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unscheduledBuiltins names handler types in this package that are
// deliberately NOT registered as built-in jobs, each with the reason.
//
// An entry here is a claim somebody has to justify, which is the point: the
// alternative to an explicit list is the implicit one that let the TTL sweeper
// ship unregistered for months while `ttl_field` quietly did nothing.
var unscheduledBuiltins = map[string]string{
	"RemoteHandler": "" +
		"Not a built-in. Registered per-job from ATL_JOBS_REMOTE_HANDLERS, " +
		"forwarding to a caller's out-of-process worker.",
}

// Every job handler in this package must be either scheduled as a built-in or
// listed above with a reason.
//
// This is the test that would have caught the sweeper. It reads the source
// rather than the runtime because there is nothing to inspect at runtime — an
// unregistered handler is a type nobody constructs, indistinguishable from a
// registered one until the moment it fails to run.
func TestEveryHandlerIsScheduledOrExplicitlyNot(t *testing.T) {
	// Every .go file in the directory, build tags included. A handler behind a
	// build tag is still a handler somebody has to decide about, and skipping
	// one would reintroduce exactly the blind spot this test exists to close.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files = append(files, f)
	}

	handlers := map[string]bool{}    // types declaring Handle(ctx, []byte) error
	constructed := map[string]bool{} // types built inside Builtins()

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil && fn.Name.Name == "Handle" && isJobHandleSig(fn.Type) {
				if name := receiverTypeName(fn.Recv); name != "" {
					handlers[name] = true
				}
				continue
			}
			if fn.Recv == nil && fn.Name.Name == "Builtins" {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if lit, ok := n.(*ast.CompositeLit); ok {
						if id, ok := lit.Type.(*ast.Ident); ok {
							constructed[id.Name] = true
						}
					}
					return true
				})
			}
		}
	}

	if len(handlers) == 0 {
		t.Fatal("found no job handlers at all, so this test asserts nothing. The " +
			"signature match or the parse is wrong, not the package")
	}
	if !handlers["ReapParkedHandler"] {
		t.Error("ReapParkedHandler was not recognised as a handler, so the scan " +
			"misses real handlers and would not catch an unregistered one")
	}

	for name := range handlers {
		if constructed[name] {
			if reason, listed := unscheduledBuiltins[name]; listed {
				t.Errorf("%s is both scheduled in Builtins() and listed as "+
					"unscheduled (%q). One of the two is wrong", name, reason)
			}
			continue
		}
		if _, listed := unscheduledBuiltins[name]; listed {
			continue
		}
		t.Errorf("%s is a job handler that Builtins() never constructs, so nothing "+
			"registers it and nothing schedules it: it will never run, and it will "+
			"not complain. Either add it to Builtins() or add it to "+
			"unscheduledBuiltins with the reason it does not belong there", name)
	}

	// The reasons carry the whole weight of an exemption, so an empty one is
	// not an exemption.
	for name, reason := range unscheduledBuiltins {
		if len(strings.TrimSpace(reason)) < 20 {
			t.Errorf("unscheduledBuiltins[%q] has no real reason. The list is only "+
				"worth having if each entry says why", name)
		}
		if !handlers[name] {
			t.Errorf("unscheduledBuiltins names %q, which is not a handler in this "+
				"package. A stale exemption hides the next real one", name)
		}
	}
}

// Every built-in needs a name, a cadence, a constructor and a stated purpose.
func TestBuiltinsAreFullyDescribed(t *testing.T) {
	all := Builtins()
	// A range over an empty slice asserts nothing while reporting success, and
	// the AST test above cannot see it: that one reads the composite literal in
	// the source, so the list and what the function actually RETURNS can differ
	// without either test noticing.
	if len(all) == 0 {
		t.Fatal("Builtins() returned nothing, so every assertion below is a loop " +
			"over an empty slice. No built-in job would be registered or scheduled")
	}
	var haveReaper bool
	for _, b := range all {
		if b.Name == ReapParkedJobName {
			haveReaper = true
		}
	}
	if !haveReaper {
		t.Errorf("%s is not in Builtins(), so parked objects are never reaped", ReapParkedJobName)
	}

	seen := map[string]bool{}
	for _, b := range all {
		switch {
		case b.Name == "":
			t.Error("a built-in has no name, so it cannot be scheduled or looked up")
		case b.CronSpec == "":
			t.Errorf("%s has no cron spec: it would be registered and never fire", b.Name)
		case b.New == nil:
			t.Errorf("%s has no constructor", b.Name)
		case strings.TrimSpace(b.Why) == "":
			t.Errorf("%s does not say what it is for. Somebody looking at a "+
				"schedule row needs to know whether they can disable it", b.Name)
		}
		if seen[b.Name] {
			t.Errorf("%s is listed twice; the second registration silently "+
				"replaces the first", b.Name)
		}
		seen[b.Name] = true
		if !strings.HasPrefix(b.Name, "atlantis.") {
			t.Errorf("%s is not namespaced under atlantis., so it can collide "+
				"with a caller's job name", b.Name)
		}
	}
}

// isJobHandleSig reports whether a method signature is Handle(context.Context,
// []byte) error — the job runtime's contract.
func isJobHandleSig(ft *ast.FuncType) bool {
	if ft.Params == nil || len(ft.Params.List) != 2 {
		return false
	}
	if ft.Results == nil || len(ft.Results.List) != 1 {
		return false
	}
	sel, ok := ft.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Context" {
		return false
	}
	arr, ok := ft.Params.List[1].Type.(*ast.ArrayType)
	if !ok {
		return false
	}
	if id, ok := arr.Elt.(*ast.Ident); !ok || id.Name != "byte" {
		return false
	}
	id, ok := ft.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "error"
}

func receiverTypeName(recv *ast.FieldList) string {
	if len(recv.List) == 0 {
		return ""
	}
	switch t := recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}
