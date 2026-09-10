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
	"sort"
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
const minCloudActions = 10

// TestEveryCloudAuditActionIsProjected reads the action argument of every
// LogAction call and compares the set with analytics.CloudActions.
func TestEveryCloudAuditActionIsProjected(t *testing.T) {
	found := auditActions(t, cloudAuditRoots, auditEntryPoints)

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
func auditActions(t *testing.T, roots []string, entryPoints map[string]int) map[string]string {
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
					actionArg, isEntry := entryPoints[calleeName(call)]
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

// forwards reports whether an expression is a parameter of the enclosing
// function or of a closure inside it, which means the literal lives at the
// callers of whichever one takes it.
//
// The console's approval handler builds `audit := func(verb string)` and hands
// verb to logAction, so a walk that read only the FuncDecl's own parameters
// would report that call as an action it could not resolve.
func forwards(fn *ast.FuncDecl, e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	found := false
	collect := func(params *ast.FieldList) {
		if params == nil {
			return
		}
		for _, field := range params.List {
			for _, name := range field.Names {
				if name.Name == id.Name {
					found = true
				}
			}
		}
	}
	collect(fn.Type.Params)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			collect(lit.Type.Params)
		}
		return true
	})
	return found
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

// catalogueFile is the file the projections are read from.
const catalogueFile = "../../internal/analytics/catalogue.go"

// minProjectionsWithProps is the floor a broken parse of the catalogue falls
// through.
const minProjectionsWithProps = 4

// TestEveryProjectionReadsOnlyKeysItsCallSitesWrite compares the detail keys a
// projection reads with the keys its call sites write.
//
// str and num return the zero value for an absent key, so a projection reading
// a key nobody writes emits a zero that reads as data.
//
// A key is required from every call site of an action, not any: one that omits
// it emits the zero on that path.
func TestEveryProjectionReadsOnlyKeysItsCallSitesWrite(t *testing.T) {
	written := detailKeys(t, cloudAuditRoots, auditEntryPoints)
	read := projectionReads(t, catalogueFile, "CloudActions")

	if len(read) < minProjectionsWithProps {
		t.Fatalf("found %d projections with properties, want at least %d — the parse is not reading what it thinks it is",
			len(read), minProjectionsWithProps)
	}

	for _, action := range sortedKeys(read) {
		keys, ok := written[action]
		if !ok {
			t.Errorf("the catalogue projects %q, which no call site writes", action)
			continue
		}
		for _, key := range read[action] {
			if !keys[key] {
				t.Errorf("%s's projection reads %q, which its call sites do not write; the value is always the zero one",
					action, key)
			}
		}
	}
}

// detailKeys maps each action to the keys every one of its call sites writes.
//
// Intersected rather than unioned: a key one site omits is absent on that path,
// which is the same defect as a key nobody writes.
//
// A detail argument that is neither nil nor a map literal fails the test, as an
// unresolvable action argument does — the keys could not be read from it.
func detailKeys(t *testing.T, roots []string, entryPoints map[string]int) map[string]map[string]bool {
	t.Helper()
	found := map[string]map[string]bool{}

	forEachAuditCall(t, roots, entryPoints, func(fn *ast.FuncDecl, call *ast.CallExpr, actionArg int, where string) {
		actions := resolveStrings(call.Args[actionArg], stringLocals(fn))
		if len(actions) == 0 {
			return
		}

		// A closure that takes the action and holds the detail has no detail
		// argument at its call sites: the console's approval handler builds
		// `audit := func(verb string)` so its two arms share one map. The keys
		// are the ones the forwarding call inside it writes.
		detail := ast.Expr(nil)
		if len(call.Args) > actionArg+1 {
			detail = call.Args[actionArg+1]
		} else if forwarded := forwardedDetail(fn, entryPoints); forwarded != nil {
			detail = forwarded
		} else {
			t.Errorf("%s: the call carries no detail and the enclosing function forwards none, so the keys it writes cannot be read", where)
			return
		}

		keys, ok := detailLiteralKeys(detail)
		if !ok {
			t.Errorf("%s: the detail argument is neither nil nor a map literal, so the keys it writes cannot be read", where)
			return
		}
		for _, action := range actions {
			seen, ok := found[action]
			if !ok {
				// Copied: one call site resolves to two actions when its
				// action argument is a local, and a later intersection on
				// either would otherwise reach into the other.
				own := make(map[string]bool, len(keys))
				for key := range keys {
					own[key] = true
				}
				found[action] = own
				continue
			}
			for key := range seen {
				if !keys[key] {
					delete(seen, key)
				}
			}
		}
	})
	return found
}

// detailLiteralKeys returns the string keys of a detail argument.
func detailLiteralKeys(e ast.Expr) (map[string]bool, bool) {
	keys := map[string]bool{}
	if id, ok := e.(*ast.Ident); ok && id.Name == "nil" {
		return keys, true
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, false
		}
		key, ok := literal(kv.Key)
		if !ok {
			return nil, false
		}
		keys[key] = true
	}
	return keys, true
}

// projectionReads maps each action to the detail keys its Props function reads.
func projectionReads(t *testing.T, path, table string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	out := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) == 0 || spec.Names[0].Name != table || len(spec.Values) == 0 {
			return true
		}
		table, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range table.Elts {
			entry, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			action, ok := literal(entry.Key)
			if !ok {
				continue
			}
			props, ok := entry.Value.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if keys, ok := propsReads(props); ok {
				out[action] = keys
			}
		}
		return false
	})
	return out
}

// propsReads returns the keys a Projection's Props function indexes its
// argument with.
func propsReads(entry *ast.CompositeLit) ([]string, bool) {
	for _, field := range entry.Elts {
		kv, ok := field.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		name, ok := kv.Key.(*ast.Ident)
		if !ok || name.Name != "Props" {
			continue
		}
		fn, ok := kv.Value.(*ast.FuncLit)
		if !ok || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 ||
			len(fn.Type.Params.List[0].Names) == 0 {
			return nil, false
		}
		param := fn.Type.Params.List[0].Names[0].Name

		var keys []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			idx, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			id, ok := idx.X.(*ast.Ident)
			if !ok || id.Name != param {
				return true
			}
			if key, ok := literal(idx.Index); ok {
				keys = append(keys, key)
			}
			return true
		})
		return keys, true
	}
	return nil, false
}

// forEachAuditCall calls visit for every call to a declared audit entry point,
// skipping a forwarder whose callers hold the literals.
func forEachAuditCall(t *testing.T, roots []string, entryPoints map[string]int, visit func(fn *ast.FuncDecl, call *ast.CallExpr, actionArg int, where string)) {
	t.Helper()
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
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					actionArg, isEntry := entryPoints[calleeName(call)]
					if !isEntry || len(call.Args) <= actionArg || forwards(fn, call.Args[actionArg]) {
						return true
					}
					visit(fn, call, actionArg, fset.Position(call.Pos()).String())
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

// sortedKeys returns a map's keys in order, so a failure names them the same
// way on every run.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// consoleAuditRoots is the tree whose logAction calls ConsoleActions covers.
var consoleAuditRoots = []string{"../../internal/console"}

// consoleEntryPoints are the console's audit writers and the argument the
// action sits in.
//
// logAction is the writer, on orgStore. audit is the closure the approval
// handler builds so its two arms share one detail map; it takes the action
// first and its callers hold the literals.
var consoleEntryPoints = map[string]int{
	"logAction": 3,
	"audit":     0,
}

// minConsoleActions is the floor a broken walk falls through.
const minConsoleActions = 28

// TestEveryConsoleAuditActionIsProjected holds ConsoleActions and
// internal/console level with each other, as its Cloud counterpart does.
func TestEveryConsoleAuditActionIsProjected(t *testing.T) {
	found := auditActions(t, consoleAuditRoots, consoleEntryPoints)

	if len(found) < minConsoleActions {
		t.Fatalf("found %d console audit actions, want at least %d — the walk is not reading what it thinks it is",
			len(found), minConsoleActions)
	}

	for action, where := range found {
		if _, ok := analytics.ConsoleActions[action]; !ok {
			t.Errorf("console audit action %q at %s has no projection in internal/analytics/catalogue.go",
				action, where)
		}
	}
	for action, p := range analytics.ConsoleActions {
		if _, ok := found[action]; !ok {
			t.Errorf("ConsoleActions projects %q, which no logAction call writes — a rename left it dangling",
				action)
		}
		if p.Event == "" && p.Why == "" {
			t.Errorf("console action %q is not reported and says no reason; set Why", action)
		}
	}
}

// TestNoConsoleProjectionLeaksASensitiveValue runs every console projection
// over a detail map carrying one of everything those call sites write.
//
// The console records operator work on a customer's own schema, so the values
// here are the customer's: entity patterns, caller names, OIDC subjects,
// certificate fingerprints, remote addresses and typed reasons.
func TestNoConsoleProjectionLeaksASensitiveValue(t *testing.T) {
	secrets := map[string]string{
		"caller":          "billing-api",
		"pattern":         "shop.Order",
		"aliases":         "orders-writer",
		"reason":          "because the customer asked in ticket 4471",
		"subject":         "ada@example.com",
		"subject_pattern": "*@example.com",
		"issuer":          "https://accounts.example.com",
		"fingerprint":     "SHA256:0f2a9c",
		"presented_cn":    "billing-api.acme",
		"remote":          "203.0.113.7:44212",
		"sandbox":         "sb_01H9",
		"forked_from":     "sb_01H8",
		"schema_version":  "9f3c1d2b",
		"plan_id":         "plan_01H9",
		"rule_id":         "rule_01H9",
		"job_id":          "job_01H9",
		"session_id":      "sess_01H9",
		"import":          "imp_01H9",
		"actor":           "user_01H9",
		"org":             "acme",
		"id":              "fw_01H9",
	}

	detail := map[string]any{
		"can_mutate":            true,
		"developers_may_enroll": true,
		"rehearsal_enabled":     true,
		"replaced":              true,
		"revoked":               3,
		"failures":              1,
		"sessions_removed":      2,
		"ttl_seconds":           900,
		"to_version":            7,
		"role":                  "approver",
		"verdict":               "pass",
		"floor":                 "always_ask",
		"apply_policy":          "auto_safe",
		"backend":               "sim",
		"entities":              []string{"shop.Order", "shop.Customer"},
		"entries":               []string{"shop.Order"},
		"sandboxes":             []string{"sb_01H9", "sb_01HA"},
		"expires_at":            "2026-10-01T00:00:00Z",
		"starts_at":             "2026-10-01T00:00:00Z",
		"ends_at":               "2026-10-02T00:00:00Z",
	}
	for k, v := range secrets {
		detail[k] = v
	}

	for action, p := range analytics.ConsoleActions {
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

// TestEveryConsoleProjectionReadsOnlyKeysItsCallSitesWrite is the Cloud drift
// check over the console's table.
func TestEveryConsoleProjectionReadsOnlyKeysItsCallSitesWrite(t *testing.T) {
	written := detailKeys(t, consoleAuditRoots, consoleEntryPoints)
	read := projectionReads(t, catalogueFile, "ConsoleActions")

	if len(read) < minProjectionsWithProps {
		t.Fatalf("found %d console projections with properties, want at least %d",
			len(read), minProjectionsWithProps)
	}

	for _, action := range sortedKeys(read) {
		keys, ok := written[action]
		if !ok {
			t.Errorf("ConsoleActions projects %q, which no call site writes", action)
			continue
		}
		for _, key := range read[action] {
			if !keys[key] {
				t.Errorf("%s's projection reads %q, which its call sites do not write; the value is always the zero one",
					action, key)
			}
		}
	}
}

// calleeName is the function a call names, for a method on a value and for a
// plain identifier.
//
// The console's approval handler calls a local closure, audit("..."), whose
// Fun is an Ident; every other audit writer is reached through a selector.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fn.Sel.Name
	case *ast.Ident:
		return fn.Name
	default:
		return ""
	}
}

// forwardedDetail returns the detail map of a call in fn whose action argument
// is a parameter rather than a literal.
//
// That call is the one a closure makes on behalf of its own callers, so its
// map is what every action those callers name actually writes.
func forwardedDetail(fn *ast.FuncDecl, entryPoints map[string]int) ast.Expr {
	var detail ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		actionArg, isEntry := entryPoints[calleeName(call)]
		if !isEntry || len(call.Args) <= actionArg+1 {
			return true
		}
		if _, literal := literal(call.Args[actionArg]); literal {
			return true
		}
		if forwards(fn, call.Args[actionArg]) {
			detail = call.Args[actionArg+1]
		}
		return true
	})
	return detail
}
