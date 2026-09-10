// Holds the join between a page and the route that sanitises what it sends.
//
// The module in web/shared is where the payload is shaped and the identity is
// held; a page reaching the ingestion route by itself gets neither, and the
// vitest suite beside that module would still pass.
package analytics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ingestionServers are the packages that serve browser ingestion. Each is a
// separate origin: `connect-src 'self'` reaches only the one that served the
// page.
var ingestionServers = []string{
	"../../internal/cloud/server",
	"../../internal/console",
}

// TestEveryBrowserOriginServesIngestionUnauthenticated reads each server for
// the route registration.
//
// The handler must be the proxy itself. Wrapping it in a session check would
// drop the sign-up funnel, which is measured before a session exists, and the
// console's sign-in page, which is where somebody blocked at sign-in is.
func TestEveryBrowserOriginServesIngestionUnauthenticated(t *testing.T) {
	for _, dir := range ingestionServers {
		found := false
		forEachGoFile(t, dir, func(fset *token.FileSet, file *ast.File) {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Handle" {
					return true
				}
				if !mentions(call.Args[0], "ProxyPathPrefix") {
					return true
				}
				found = true
				if _, ok := call.Args[1].(*ast.SelectorExpr); !ok {
					t.Errorf("%s registers ingestion with %T rather than the proxy itself; "+
						"a wrapper here is a check a page with no session cannot pass",
						fset.Position(call.Pos()), call.Args[1])
				}
				return true
			})
		})
		if !found {
			t.Errorf("%s serves a browser and registers no ingestion route, so its "+
				"pages report nothing", dir)
		}
	}
}

// The module every page reports through, and the file that tests it. Nothing
// else may name the ingestion route.
const (
	sharedSender    = "../../web/shared/analytics.ts"
	sharedSenderTst = "../../web/shared/analytics.test.ts"
)

// browserApps are the applications that must report, each with the directory
// walked for the import.
var browserApps = map[string]string{
	"console": "../../web/console/src",
	"cloud":   "../../web/cloud/src",
}

// TestTheIngestionLimiterCarriesTheIngestionBudget reads the limiter each
// server hands the proxy.
//
// Both deployments keep one limiter type and size it per route, so the field
// wired to Allow is the only thing separating a page sending an event every
// few seconds from a sign-in budget measured in tens per minute. Handing over
// the wrong limiter throttles browser analytics to almost nothing, and every
// other test in the tree passes.
func TestTheIngestionLimiterCarriesTheIngestionBudget(t *testing.T) {
	for _, dir := range ingestionServers {
		field := ""
		sized := map[string]bool{}

		forEachGoFile(t, dir, func(fset *token.FileSet, file *ast.File) {
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					// The ProxyConfig literal: Allow is `<x>.<field>.allow`.
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Allow" {
							continue
						}
						if name := limiterField(kv.Value); name != "" {
							field = name
						}
					}
					// Any `<field>: newXWithMax(ProxyRateLimit)` in the tree.
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := kv.Key.(*ast.Ident)
						if !ok {
							continue
						}
						if mentions(kv.Value, "ProxyRateLimit") && mentions(kv.Value, "ProxyMaxAddresses") {
							sized[key.Name] = true
						}
					}
				}
				return true
			})
		})

		switch {
		case field == "":
			t.Errorf("%s hands the proxy no limiter field, so this asserts nothing", dir)
		case !sized[field]:
			t.Errorf("%s gives the ingestion proxy %q, which is not built with "+
				"analytics.ProxyRateLimit; browser analytics runs on another route's budget",
				dir, field)
		}
	}
}

// limiterField returns the field name in an expression shaped
// `<receiver>.<field>.allow`, or "".
func limiterField(e ast.Expr) string {
	outer, ok := e.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != "allow" {
		return ""
	}
	inner, ok := outer.X.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return inner.Sel.Name
}

// TestOnlyTheSharedModuleReachesIngestion finds every file that posts to the
// route.
//
// One sender is what keeps the route pattern, the identity and the absence of
// a project key true of every event rather than of the events one file happens
// to send.
func TestOnlyTheSharedModuleReachesIngestion(t *testing.T) {
	var sawSender bool

	for _, root := range append(dirs(browserApps), "../../web/shared") {
		forEachSource(t, root, func(path, text string) {
			if strings.Contains(text, "posthog-js") {
				t.Errorf("%s loads posthog-js, which is served from a third-party origin "+
					"that `script-src 'self'` refuses", path)
			}
			if !strings.Contains(text, "/api/t/") {
				return
			}
			switch filepath.ToSlash(path) {
			case sharedSender:
				sawSender = true
			case sharedSenderTst:
			default:
				t.Errorf("%s posts to the ingestion route directly, so what it sends is "+
					"shaped by neither the module nor its tests", path)
			}
		})
	}

	if !sawSender {
		t.Fatalf("%s does not name the ingestion route, so this asserts nothing — "+
			"either the walk is broken or the module has moved", sharedSender)
	}
}

// TestEveryBrowserReportsThroughTheSharedModule finds the import in each
// application.
//
// Without it the walk above passes on a tree where every page wiring has been
// deleted: web/shared alone satisfies "something names the route", and the
// browsers report nothing.
func TestEveryBrowserReportsThroughTheSharedModule(t *testing.T) {
	const module = "@atlantis/shared/analytics"

	for app, root := range browserApps {
		var importers []string
		forEachSource(t, root, func(path, text string) {
			if strings.Contains(text, module) {
				importers = append(importers, path)
			}
		})
		if len(importers) == 0 {
			t.Errorf("%s imports %s nowhere, so its pages report nothing", app, module)
		}
	}
}

// dirs returns the values of m, so a map of applications can be walked.
func dirs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// forEachSource reads every TypeScript file under root.
func forEachSource(t *testing.T, root string, fn func(path, text string)) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fn(path, string(raw))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// mentions reports whether an expression names an identifier, through however
// many concatenations.
func mentions(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// forEachGoFile parses every non-test file directly in dir.
func forEachGoFile(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		fn(fset, file)
	}
}
