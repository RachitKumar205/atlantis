package admin

import (
	"go/ast"
	"os"
	"strings"
	"testing"
)

// ApplyMigration must refuse a destructive plan on the SERVER.
//
// # What this protects
//
// Until now the only class check in ApplyMigration was CrossCallerBreaking. A
// destructive plan was stopped by cmd/tide/apply.go deciding not to call this
// RPC — so the gate lived in the client. Anything that skipped that switch
// applied the DROP: a script, a retry wrapper, a newer or older tide, or any
// direct gRPC client holding CAPABILITY_SCHEMA_APPLY. The interceptor checks
// the capability and nothing else; it cannot see the request body, so it has no
// idea the change is destructive.
//
// classifyRemoval (internal/codegen/diff.go) deliberately floors an owned
// removal at ClassDestructive rather than ClassAdditive, with the reasoning
// that "owning the thing you are deleting says nothing about whether the
// deletion loses data". That floor was doing nothing on the server.
//
// # Why this reads source, and what replaces it
//
// Same reason TestApplyMigrationRefusesInvalidCustomSQL does: ApplyMigration
// needs a database, a caller identity, a prior checkpoint and a matching plan
// ID before it reaches this line, and a test that assembled all of that would
// be testing the fixture.
//
// That is a real limit, not a preference — this asserts the condition is
// present, never that it fires. TestDirectApplyCannotSkipApproval, which calls
// the handler against a live database, is the one that proves it, and it
// arrives with the change-policy work. Until then this stops the arm being
// quietly dropped.
func TestApplyMigrationRefusesDestructivePlansServerSide(t *testing.T) {
	_, files := parsePackageSources(t)

	// Matched on the receiver, not the name alone: grpcgen.go carries a second
	// ApplyMigration that delegates here, and selecting by name let map
	// iteration order decide which body was inspected.
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
			"cannot tell whether destructive plans are refused")
	case found > 1:
		t.Fatalf("%d ApplyMigration methods on *Service; this test inspects one "+
			"body and would be checking an arbitrary one of them", found)
	}

	// The refusal must name both classes. Either one alone leaves a hole:
	// without Destructive, an owned column drop applies unattended through any
	// client that is not tide; without CrossCallerBreaking, a change another
	// caller reads applies without them knowing.
	var sawBreaking, sawDestructive bool
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, isIdent := sel.X.(*ast.Ident)
		if !isIdent || pkg.Name != "codegen" {
			return true
		}
		switch sel.Sel.Name {
		case "ClassCrossCallerBreaking":
			sawBreaking = true
		case "ClassDestructive":
			sawDestructive = true
		}
		return true
	})

	if !sawBreaking {
		t.Error("ApplyMigration does not mention codegen.ClassCrossCallerBreaking. " +
			"A change another caller reads would apply without them knowing")
	}
	if !sawDestructive {
		t.Error("ApplyMigration does not mention codegen.ClassDestructive. The " +
			"refusal then lives only in cmd/tide's switch, so any direct gRPC " +
			"client holding CAPABILITY_SCHEMA_APPLY can drop a column — the " +
			"capability interceptor cannot see the request body and has no idea " +
			"the change is destructive")
	}

	// And the mention has to be a refusal, not a log line or a metric label.
	var returnsOnIt bool
	ast.Inspect(body, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		var mentions bool
		ast.Inspect(stmt.Cond, func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "ClassDestructive" {
				mentions = true
			}
			return true
		})
		if !mentions {
			return true
		}
		for _, s := range stmt.Body.List {
			if _, isReturn := s.(*ast.ReturnStmt); isReturn {
				returnsOnIt = true
			}
		}
		return true
	})
	if !returnsOnIt {
		t.Error("codegen.ClassDestructive appears in ApplyMigration but not in a " +
			"condition that returns. The class is computed and the apply " +
			"proceeds anyway")
	}
}

// The refusal message must name which class stopped the apply.
//
// "plan is breaking and cannot be auto-applied" was accurate when breaking was
// the only refused class. With two, a fixed string tells an operator dropping
// their own column that they broke another caller — sending them to coordinate
// with a team that is not involved.
func TestDestructiveRefusalNamesTheClass(t *testing.T) {
	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatalf("read admin.go: %v", err)
	}
	if !strings.Contains(string(src), `"admin: plan is %s and cannot be auto-applied"`) {
		t.Error("the apply refusal does not interpolate the class. An operator " +
			"dropping their own column is told the change is breaking other " +
			"callers, and goes looking for a team that has nothing to do with it")
	}
}
