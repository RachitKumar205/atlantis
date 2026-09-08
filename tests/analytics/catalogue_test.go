// Holds the audit log and the analytics catalogue level with each other.
//
// Every action written to an audit log has a Projection, and every Projection
// has a call site. Without both directions a new audit action reports nothing
// and nobody notices, or a renamed one leaves an entry that can never fire.
//
// The projections themselves are the privacy control: no path copies an audit
// detail map through, so a key nobody named cannot leave the process. The
// fixture at the bottom is what proves that.
package analytics_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// cloudAuditRoots are the trees whose LogAction calls the catalogue must cover.
var cloudAuditRoots = []string{
	"../../internal/cloud",
	"../../internal/provisioner",
}

// auditEntryPoints are the functions that write an audit action, and the
// argument position the action sits in.
//
// LogAction is the writer. audit is the provisioner's forwarder, which passes
// its own parameter through, so its callers hold the literals.
// TestNoUnknownAuditForwarderExists fails when a third one appears.
var auditEntryPoints = map[string]int{
	"LogAction": 4,
	"audit":     2,
}

// minCloudActions is the floor a broken walk falls through.
//
// Without it a parser change that stops matching anything leaves every
// assertion below iterating an empty set and passing, which is the failure
// mode this file exists to prevent.
const minCloudActions = 8

// TestEveryCloudAuditActionIsProjected reads the action argument of every
// LogAction call and compares the set with analytics.CloudActions.
func TestEveryCloudAuditActionIsProjected(t *testing.T) {
	found := auditActions(t, cloudAuditRoots)

	if len(found) < minCloudActions {
		t.Fatalf("found %d audit actions, want at least %d — the walk is not reading what it thinks it is",
			len(found), minCloudActions)
	}

	for action, where := range found {
		if _, ok := analytics.CloudActions[action]; !ok {
			t.Errorf("audit action %q at %s has no projection in internal/analytics/catalogue.go",
				action, where)
		}
	}

	for action, p := range analytics.CloudActions {
		if _, ok := found[action]; !ok {
			t.Errorf("catalogue projects %q, which no LogAction call writes — a rename left it dangling",
				action)
		}
		if p.Event == "" && p.Why == "" {
			t.Errorf("action %q is not reported and says no reason; set Why", action)
		}
	}
}

// TestNoProjectionLeaksASensitiveValue runs every projection over a detail map
// carrying one of everything an audit row has been seen to hold.
//
// The values are what must never reach a third party: customer schema, a
// cluster address, a device code, an email, a token.
func TestNoProjectionLeaksASensitiveValue(t *testing.T) {
	secrets := map[string]string{
		"up_sql":       "ALTER TABLE customer_orders ADD COLUMN ssn text",
		"down_sql":     "DROP COLUMN ssn",
		"ir_snapshot":  "{\"entities\":[{\"name\":\"CustomerOrder\"}]}",
		"files":        "schema.atl",
		"last_error":   "Get \"https://10.24.0.1:443/api/v1/namespaces\": connection refused",
		"error":        "Get \"https://10.24.0.1:443/api/v1/namespaces\": connection refused",
		"endpoint":     "org-acme.tryatlantis.dev:31234",
		"reason":       "because the customer asked in ticket 4471",
		"email":        "ada@example.com",
		"actor_email":  "ada@example.com",
		"token":        "atl_secret_token_value",
		"user_code":    "8FWL-RHTP",
		"caller":       "billing-api",
		"display_name": "Ada's Private Holdings Ltd",
		"hostname":     "ada-laptop.internal",
	}

	detail := map[string]any{
		"attempts":       3,
		"took_ms":        48000,
		"retry_in":       "30s",
		"restorable_for": "720h0m0s",
	}
	for k, v := range secrets {
		detail[k] = v
	}

	for action, p := range analytics.CloudActions {
		if p.Props == nil {
			continue
		}
		props := p.Props(detail)
		encoded, err := json.Marshal(props)
		if err != nil {
			t.Fatalf("%s: properties do not encode: %v", action, err)
		}
		for key, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				t.Errorf("%s emits the value of %q:\n%s", action, key, encoded)
			}
		}
	}
}

// TestErrorClassNamesNoDetail buckets messages rather than passing them on.
func TestErrorClassNamesNoDetail(t *testing.T) {
	cases := map[string]string{
		"":                          analytics.ClassNone,
		"context deadline exceeded": analytics.ClassTimeout,
		"pod not ready within 5m0s": analytics.ClassTimeout,
		"Failed to pull image \"us-docker.pkg.dev/x\": ErrImagePull": analytics.ClassImagePull,
		"pods \"x\" is forbidden: exceeded quota: cpu":               analytics.ClassQuota,
		"Get \"https://10.24.0.1:443/api\": connection refused":      analytics.ClassKubeAPI,
		"persistentvolumeclaim \"data\" not bound":                   analytics.ClassStorage,
		"something nobody has classified yet":                        analytics.ClassOther,
	}
	for msg, want := range cases {
		if got := analytics.ErrorClass(msg); got != want {
			t.Errorf("ErrorClass(%q) = %q, want %q", msg, got, want)
		}
	}

	// Whatever the class, the message is not in it.
	msg := "Get \"https://10.24.0.1:443/api/v1/namespaces/org-acme\": connection refused"
	if got := analytics.ErrorClass(msg); strings.Contains(msg, got) && got != "" {
		t.Errorf("the class %q appears inside the message it came from", got)
	}
}

// auditActions maps every action string a LogAction call writes to where it
// was found.
//
// A call whose action argument is a variable is resolved against the string
// literals assigned to that name in the same function. An argument that
// resolves to nothing fails the test rather than being skipped, so a third way
// of naming an action cannot pass unnoticed.
func auditActions(t *testing.T, roots []string) map[string]string {
	t.Helper()
	found := map[string]string{}

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}

			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				locals := stringLocals(fn)

				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					actionArg, isEntry := auditEntryPoints[sel.Sel.Name]
					if !isEntry || len(call.Args) <= actionArg {
						return true
					}
					where := fset.Position(call.Pos()).String()
					values := resolveStrings(call.Args[actionArg], locals)
					if len(values) == 0 {
						// A forwarder passing its own parameter through is
						// covered by its own callers; anything else is an
						// action the catalogue cannot be held to.
						if !forwards(fn, call.Args[actionArg]) {
							t.Errorf("%s: the action argument is neither a literal nor a local with literal "+
								"assignments; the catalogue cannot be held to it", where)
						}
						return true
					}
					for _, v := range values {
						found[v] = where
					}
					return true
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return found
}

// stringLocals collects, per identifier, the string literals assigned to it
// anywhere in a function.
func stringLocals(fn *ast.FuncDecl) map[string][]string {
	out := map[string][]string{}
	record := func(lhs, rhs []ast.Expr) {
		for i, l := range lhs {
			id, ok := l.(*ast.Ident)
			if !ok || i >= len(rhs) {
				continue
			}
			if s, ok := literal(rhs[i]); ok {
				out[id.Name] = append(out[id.Name], s)
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			record(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if i < len(v.Values) {
					if s, ok := literal(v.Values[i]); ok {
						out[name.Name] = append(out[name.Name], s)
					}
				}
			}
		}
		return true
	})
	return out
}

// resolveStrings returns the literals an expression can be.
func resolveStrings(e ast.Expr, locals map[string][]string) []string {
	if s, ok := literal(e); ok {
		return []string{s}
	}
	if id, ok := e.(*ast.Ident); ok {
		return locals[id.Name]
	}
	// A package-level constant reaches here as a selector; the catalogue keys
	// on the literal, so an action named that way needs its own resolution.
	return nil
}

func literal(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// forwards reports whether an expression is one of the enclosing function's
// own parameters, which means the literal lives at its callers.
func forwards(fn *ast.FuncDecl, e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok || fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			if name.Name == id.Name {
				return true
			}
		}
	}
	return false
}

// TestNoUnknownAuditForwarderExists fails when a function passes its own
// parameter to LogAction without being a declared entry point.
//
// Adding one is how the totality check above quietly stops covering a tree:
// its callers hold the literals and nothing reads them.
func TestNoUnknownAuditForwarderExists(t *testing.T) {
	for _, root := range cloudAuditRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "LogAction" || len(call.Args) <= 4 {
						return true
					}
					if !forwards(fn, call.Args[4]) {
						return true
					}
					if _, known := auditEntryPoints[fn.Name.Name]; !known {
						t.Errorf("%s: %s forwards its own parameter to LogAction but is not in auditEntryPoints, "+
							"so the actions its callers write are never checked",
							fset.Position(call.Pos()), fn.Name.Name)
					}
					return true
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
