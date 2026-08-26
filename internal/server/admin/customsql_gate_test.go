package admin

import (
	"go/ast"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ApplyMigration must refuse when custom SQL fails validation, not merely
// report it.
//
// This is the last link in a chain, and the only one that stands between a
// query body and the database. sqlvalidate rejects set_config in caller SQL —
// which is what keeps a `partition by` policy meaningful, since PostgreSQL
// cannot lock the run-time parameter the policy compares against. That
// rejection reaches production through exactly one path: validateCustomSQL
// returns messages, and ApplyMigration turns them into an error.
//
// PlanSchema calls the same function and does not refuse, correctly: a plan
// shows every problem at once rather than stopping at the first. So enforcement
// rests on this one call site behaving differently from the other. Softening it
// to a warning — which is
// the natural thing to do the first time a legitimate query trips the gate —
// would leave every test in the repository green.
//
// Read from source because ApplyMigration needs a database, a caller identity,
// a prior checkpoint and a matching plan ID before it reaches this line. A test
// that assembled all of that would be testing the fixture.
func TestApplyMigrationRefusesInvalidCustomSQL(t *testing.T) {
	_, files := parsePackageSources(t)

	// Matched on the receiver, not on the name alone. grpcgen.go carries a
	// second ApplyMigration — the transport shim that delegates here — and
	// selecting by name let map iteration order decide which body got
	// inspected. The test then passed or failed at random, which is worse than
	// either.
	var body *ast.BlockStmt
	var found int
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "ApplyMigration" || fn.Recv == nil {
				continue
			}
			if receiverTypeName(fn) != "Service" {
				continue
			}
			body = fn.Body
			found++
		}
	}
	switch {
	case found == 0:
		t.Fatal("admin has no ApplyMigration method on *Service, so this test " +
			"cannot tell whether custom SQL validation blocks an apply")
	case found > 1:
		t.Fatalf("%d ApplyMigration methods on *Service; this test inspects one "+
			"body and would be checking an arbitrary one of them", found)
	}

	// An `if` whose condition mentions validateCustomSQL and whose block
	// returns. Presence of the call alone is not enough — assigning its result
	// and carrying on is exactly the softening this guards against.
	var refuses bool
	ast.Inspect(body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		var mentions bool
		inspect := func(node ast.Node) {
			ast.Inspect(node, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == "validateCustomSQL" {
					mentions = true
				}
				return true
			})
		}
		if ifs.Init != nil {
			inspect(ifs.Init)
		}
		inspect(ifs.Cond)
		if !mentions {
			return true
		}
		// A DIRECT statement of the if body, not any return anywhere beneath
		// it. Wrapping the return in a second condition that can never hold —
		// `if len(msgs) < 0 { return ... }` — satisfies a recursive search
		// while removing the enforcement, and that mutation survived until
		// this was narrowed.
		for _, stmt := range ifs.Body.List {
			if _, isReturn := stmt.(*ast.ReturnStmt); isReturn {
				refuses = true
			}
		}
		return true
	})

	if !refuses {
		t.Error("Service.ApplyMigration does not return when validateCustomSQL reports a " +
			"problem. Custom SQL that fails validation is then applied and stored: " +
			"a query body calling set_config would be able to rebind the tenant " +
			"that every `partition by` policy compares against, and PostgreSQL " +
			"offers no way to stop it at the database layer")
	}
}

// receiverTypeName returns a method's receiver type name without its pointer
// star, or "" for a non-method.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// The entity gate on the branch production takes.
//
// Every pre-existing validateCustomSQL test passes a nil ownership map, which
// takes the `len(owns) > 0 == false` path — so `if true { continue }` in the
// entity loop disabled the CHECK gate at its only server call site with the
// whole repository green. PlanSchema and ApplyMigration always pass a populated
// map, so the branch under test here is the only one that ever runs in
// production and was the only one nothing covered.
func TestValidateCustomSQL_GatesEntityChecksForTheSubmittingCaller(t *testing.T) {
	const hostile = `set_config('atlantis.tenant','victim',true) IS NOT NULL OR body IS NOT NULL`

	ir := &dsl.IR{Entities: []dsl.Entity{
		{
			Name: "Mine", Namespace: "shop",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "body", Type: dsl.FieldType{Name: "text"}},
			},
			Checks: []dsl.TableCheck{{Name: "ck", Expr: hostile}},
		},
		{
			Name: "Theirs", Namespace: "other",
			Fields: []dsl.Field{
				{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
				{Name: "body", Type: dsl.FieldType{Name: "text"}},
			},
			Checks: []dsl.TableCheck{{Name: "ck", Expr: hostile}},
		},
	}}
	owns := map[string]string{
		"shop.Mine":    "shop",
		"other.Theirs": "someone-else",
	}

	msgs := validateCustomSQL(ir, "shop", owns)
	if len(msgs) == 0 {
		t.Fatal("the submitting caller's own CHECK expression calls set_config " +
			"and was accepted. This is the branch PlanSchema and ApplyMigration " +
			"take on every request")
	}
	joined := strings.Join(msgs, "\n")
	if !strings.Contains(joined, "shop.Mine") {
		t.Errorf("did not name the offending entity: %s", joined)
	}
	// Another caller's stored content is out of scope: it was judged when its
	// owner applied it, and re-judging it now lets their staleness block this
	// apply. Asserted so the scoping is a decision rather than an accident.
	if strings.Contains(joined, "other.Theirs") {
		t.Errorf("validated another caller's entity, which turns their stored "+
			"content into a block on this caller's apply: %s", joined)
	}
}
