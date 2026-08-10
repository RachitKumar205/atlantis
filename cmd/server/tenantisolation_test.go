package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// What ATL_REQUIRE_TENANT_ISOLATION actually does, asserted by calling it.
//
// The three inputs that matter are the flag, whether detection succeeded, and
// what it found — and the flag has to bite on BOTH failure shapes. A role that
// bypasses RLS is the obvious one. Detection failing is the one that gets
// missed: a locked-down pg_roles or a pooler rewriting current_user leaves the
// answer unknown, and treating unknown as fine starts the server with the guard
// nominally on and never actually asked.
//
// Every case below survived the suite while the logic sat inline in run().
func TestTenantIsolationError(t *testing.T) {
	safe := pg.RolePrivileges{Name: "atlantis"}
	super := pg.RolePrivileges{Name: "postgres", Superuser: true}
	bypass := pg.RolePrivileges{Name: "app", BypassRLS: true}
	probeErr := errors.New("permission denied for table pg_authid")

	for _, tc := range []struct {
		name     string
		privs    pg.RolePrivileges
		derr     error
		require  bool
		wantFail bool
		// wantIn is a substring the operator needs in order to act on it.
		wantIn string
	}{
		// Flag off: reports, never refuses. This is the default, and the
		// default must not break single-tenant installs that use no
		// partitioned entity.
		{name: "off/safe role", privs: safe, require: false},
		{name: "off/superuser", privs: super, require: false},
		{name: "off/bypassrls", privs: bypass, require: false},
		{name: "off/detection failed", privs: safe, derr: probeErr, require: false},

		// Flag on.
		{name: "on/safe role", privs: safe, require: true},
		{
			name: "on/superuser", privs: super, require: true,
			wantFail: true, wantIn: "superuser",
		},
		{
			name: "on/bypassrls", privs: bypass, require: true,
			wantFail: true, wantIn: "NOBYPASSRLS",
		},
		{
			name: "on/detection failed", privs: safe, derr: probeErr, require: true,
			wantFail: true, wantIn: "could not determine",
		},
		// Detection failing outranks whatever zero-valued privileges came back
		// with it. RolePrivileges{} looks like a perfectly safe role — not a
		// superuser, no BYPASSRLS — so ranking the two the other way round
		// turns a failed probe into a clean bill of health.
		{
			name:  "on/detection failed, zero privs look safe",
			privs: pg.RolePrivileges{}, derr: probeErr, require: true,
			wantFail: true, wantIn: "could not determine",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tenantIsolationError(tc.privs, tc.derr, tc.require)
			switch {
			case tc.wantFail && err == nil:
				t.Fatalf("the server would start. require=%v derr=%v privs=%+v — "+
					"ATL_REQUIRE_TENANT_ISOLATION is set and this posture provides "+
					"no tenant isolation, so booting means every `partition by` "+
					"policy is attached and inert", tc.require, tc.derr, tc.privs)
			case !tc.wantFail && err != nil:
				t.Fatalf("the server would refuse to start on a posture that is "+
					"acceptable: %v", err)
			}
			if tc.wantIn != "" && !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("refusal does not tell the operator what to fix: want a "+
					"mention of %q, got %v", tc.wantIn, err)
			}
			if tc.derr != nil && tc.wantFail && !errors.Is(err, probeErr) {
				t.Error("the refusal drops the underlying probe error, so whoever " +
					"has to fix the database cannot see why the probe failed")
			}
		})
	}
}

// The server must reach that decision at boot, on a path that can execute.
//
// Read from source because there is nothing else to read. Whether a boot check
// ran is not observable without booting against a database, and a boot test
// would not fail if the call were deleted — it would simply stop checking,
// which is indistinguishable from checking and finding nothing wrong.
//
// The walk follows calls into package-local functions rather than scanning
// run() alone. Moving a check into a helper is a normal refactor and must not
// read as deleting it; equally, a helper nothing calls is exactly the shape of
// the bug — TestServerRegistersBuiltinJobs exists because a component shipped
// complete and unreferenced.
func TestServerChecksTenantIsolationAtBoot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/server: %v", err)
	}

	// Every top-level function in the package, so the walk can follow a call
	// into one. The whole package, not main.go alone: a check moved into a
	// sibling file is still a check that runs.
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	if funcs["run"] == nil {
		t.Fatal("cmd/server has no run() function, so this test cannot tell whether " +
			"the isolation check runs on a reachable path")
	}

	// Keyed "package.Function", because the boot-time checks now come from two
	// packages and matching on the bare name would let a same-named helper in
	// either satisfy the other's entry.
	want := map[string]bool{
		"pg.DetectRolePrivileges":         false,
		"pg.RequireIsolatedRole":          false,
		"sqlvalidate.AuditForbiddenCalls": false,
	}
	// Package-local functions run() must reach. Tracked separately from the
	// pkg.Fn set because these are bare identifiers, and because the walk
	// already descends into them — descending is not the same as requiring.
	// Deleting the storedSQLAuditError call left every test green until this
	// was added: the audit still ran, its findings were simply ignored.
	wantLocal := map[string]bool{
		"tenantIsolationError": false,
		"storedSQLAuditError":  false,
	}
	// Reached is not the same as obeyed. `_ = tenantIsolationError(...)` keeps
	// the call on a live path while discarding the refusal, and that mutation
	// survived until this was added — so these must additionally be observed
	// inside an `if ... { return ... }` whose return is a direct statement of
	// the if body, not buried under a second condition that can never hold.
	wantReturned := map[string]bool{
		"tenantIsolationError": false,
		"storedSQLAuditError":  false,
	}
	seen := map[string]bool{}

	var walkFunc func(fn *ast.FuncDecl)
	var walk func(ast.Node) bool
	walk = func(n ast.Node) bool {
		// Do not descend into a branch that provably never runs.
		if ifs, ok := n.(*ast.IfStmt); ok {
			if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "false" {
				if ifs.Else != nil {
					ast.Inspect(ifs.Else, walk)
				}
				return false
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if pkgID, ok := fun.X.(*ast.Ident); ok {
				key := pkgID.Name + "." + fun.Sel.Name
				if _, tracked := want[key]; tracked {
					want[key] = true
				}
			}
		case *ast.Ident:
			if _, tracked := wantLocal[fun.Name]; tracked {
				wantLocal[fun.Name] = true
			}
			// A call to a function declared in this package: follow it.
			if callee, ok := funcs[fun.Name]; ok {
				walkFunc(callee)
			}
		}
		return true
	}
	walkFunc = func(fn *ast.FuncDecl) {
		if seen[fn.Name.Name] {
			return
		}
		seen[fn.Name.Name] = true
		ast.Inspect(fn.Body, walk)
	}
	walkFunc(funcs["run"])

	// Second pass: reached is not obeyed. Look for each tracked decision inside
	// an `if <call>; cond { return ... }` whose return is a DIRECT statement of
	// the if body. Both evasions found by review are caught here — discarding
	// the result with `_ =`, and burying the return under a second condition
	// that can never hold.
	for name := range funcs {
		if !seen[name] {
			continue
		}
		ast.Inspect(funcs[name].Body, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Init == nil {
				return true
			}
			var called string
			ast.Inspect(ifs.Init, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					if _, tracked := wantReturned[id.Name]; tracked {
						called = id.Name
					}
				}
				return true
			})
			if called == "" {
				return true
			}
			for _, stmt := range ifs.Body.List {
				if _, isReturn := stmt.(*ast.ReturnStmt); isReturn {
					wantReturned[called] = true
				}
			}
			return true
		})
	}

	for name, ok := range wantReturned {
		if ok {
			continue
		}
		t.Errorf("cmd/server calls %s but never returns on its result. The check "+
			"runs and its answer is discarded, so the server starts on a posture "+
			"it just established was unsafe — and ATL_REQUIRE_TENANT_ISOLATION "+
			"gates nothing. Discarding with `_ =`, or burying the return under a "+
			"second condition, both look like this", name)
	}

	if !wantLocal["tenantIsolationError"] {
		t.Error("cmd/server's run() never reaches tenantIsolationError, so the " +
			"database-role posture is established and then ignored and " +
			"ATL_REQUIRE_TENANT_ISOLATION gates nothing")
	}
	if !wantLocal["storedSQLAuditError"] {
		t.Error("cmd/server's run() audits the stored schema but never reaches " +
			"storedSQLAuditError, so the findings are computed and discarded. " +
			"A checkpoint containing SQL that rebinds the caller's tenant would " +
			"be served, and ATL_REQUIRE_TENANT_ISOLATION would have nothing to " +
			"gate on")
	}
	if !want["sqlvalidate.AuditForbiddenCalls"] {
		t.Error("cmd/server's run() never audits the loaded IR checkpoint for SQL " +
			"that can rebind the caller's tenant. The validator that rejects " +
			"set_config runs at plan and apply, so it is prospective only: anything " +
			"stored before it existed is served unexamined, and a binary predating " +
			"the gate can write such a checkpoint at any time. This is the only " +
			"place that can be caught")
	}
	if !want["pg.DetectRolePrivileges"] {
		t.Error("cmd/server's run() never reaches pg.DetectRolePrivileges, so nothing " +
			"establishes whether the database role can bypass row-level security. " +
			"Every `partition by` policy would be attached and inert, and no log line " +
			"or error would say so")
	}
	if !want["pg.RequireIsolatedRole"] {
		t.Error("cmd/server's run() reads the role privileges but never calls " +
			"pg.RequireIsolatedRole, so the answer is computed and discarded. " +
			"ATL_REQUIRE_TENANT_ISOLATION would have nothing to gate on")
	}
}

// What the audit's findings actually do.
//
// The audit had tests; whether a finding stopped the server did not, and a
// mutation making the flag inert survived the whole suite. This is the same
// gap tenantIsolationError was extracted to close, found the same way.
func TestStoredSQLAuditError(t *testing.T) {
	one := []error{errors.New("stored query shop.Probe: SQL calls set_config()")}
	two := []error{
		errors.New("shop.Doc: check ck: SQL calls set_config()"),
		errors.New("stored procedure shop.P step 1: SQL calls set_partition()"),
	}

	for _, tc := range []struct {
		name     string
		findings []error
		require  bool
		wantFail bool
	}{
		// Flag off: report, never refuse. A deployment with no partitioned
		// entity is unaffected, and refusing by default would stop every
		// existing install to protect a feature it does not use.
		{name: "off/no findings", findings: nil, require: false},
		{name: "off/one finding", findings: one, require: false},
		{name: "off/several findings", findings: two, require: false},

		// Flag on.
		{name: "on/no findings", findings: nil, require: true},
		{name: "on/empty slice is not a finding", findings: []error{}, require: true},
		{name: "on/one finding", findings: one, require: true, wantFail: true},
		{name: "on/several findings", findings: two, require: true, wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storedSQLAuditError(tc.findings, tc.require)
			switch {
			case tc.wantFail && err == nil:
				t.Fatalf("the server would start with %d finding(s) and "+
					"ATL_REQUIRE_TENANT_ISOLATION set. The stored schema can rebind "+
					"the tenant every `partition by` policy compares against",
					len(tc.findings))
			case !tc.wantFail && err != nil:
				t.Fatalf("the server would refuse to start when it should not: %v", err)
			}
			if !tc.wantFail {
				return
			}
			// Every finding has to reach the operator. Reporting only the first
			// turns a schema-wide problem into a game of whack-a-mole.
			for _, f := range tc.findings {
				if !errors.Is(err, f) {
					t.Errorf("the refusal drops finding %q, so fixing the ones it "+
						"does name would not make the server start", f)
				}
			}
		})
	}
}
