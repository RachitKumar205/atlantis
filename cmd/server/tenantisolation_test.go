package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
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
		// The policy check, and the hook that repeats it on every hot reload.
		// Boot alone misses the case it exists for: a checkpoint adding
		// `partition by` to an existing entity emits no DDL and arrives at a
		// running server.
		"pg.VerifyPartitionPolicies": false,
		// Installing the hook is separate from defining it: boot can call the
		// verification and never repeat it on reload.
		"dynServer.SetOnReload": false,
		// Nothing supplies the tenant unless this is chained in. The policy,
		// the discriminator and the dispatcher's binding were all complete and
		// correct for a while with no caller for WithCallerPartition anywhere
		// in the tree, and every request on a partitioned entity failed.
		"interceptors.NewPartition":       false,
		"interceptors.NewPartitionStream": false,
	}
	// Package-local functions run() must reach. Tracked separately from the
	// pkg.Fn set because these are bare identifiers, and because the walk
	// already descends into them — descending is not the same as requiring.
	// Deleting the storedSQLAuditError call left every test green until this
	// was added: the audit still ran, its findings were simply ignored.
	wantLocal := map[string]bool{
		"tenantIsolationError": false,
		"storedSQLAuditError":  false,
		// Boot-only verification misses the delivery path the check exists for.
		"verifyPartitions": false,
		// The shared decision. pg.PartitionPolicyError is called only from
		// inside it now, so tracking that name would follow the logic rather
		// than the wiring — and the logic has its own behavioural test.
		"partitionGate": false,
	}
	// Reached is not the same as obeyed. `_ = tenantIsolationError(...)` keeps
	// the call on a live path while discarding the refusal, and that mutation
	// survived until this was added — so these must additionally be observed
	// inside an `if ... { return ... }` whose return is a direct statement of
	// the if body, not buried under a second condition that can never hold.
	wantReturned := map[string]bool{
		"tenantIsolationError": false,
		"storedSQLAuditError":  false,
		// Appears TWICE — at boot and in the reload hook — so a "was it
		// reached" check is satisfied by either one while the other is
		// discarded. Both must return, which the call-site count below asserts.
		"partitionGate": false,
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

	// Second pass: reached is not obeyed, and EVERY call site must obey.
	//
	// Counting "is it returned somewhere" is not enough. PartitionPolicyError is
	// called twice — at boot and in the reload hook — so a name-level check is
	// satisfied by either one while the other discards its result, and both
	// mutations survived until this became per-call-site.
	//
	// A call obeys if it sits in an `if <call>; cond { return ... }` whose
	// return is a direct statement of the if body, or inside a return
	// expression. Anything else — assignment to _, a bare statement, a return
	// buried under a second condition — discards the refusal.
	obeyed := map[ast.Node]bool{}
	for name := range funcs {
		if !seen[name] {
			continue
		}
		ast.Inspect(funcs[name].Body, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.IfStmt:
				if stmt.Init == nil {
					return true
				}
				// The CONDITION and the RETURNED VALUE, not merely the
				// presence of a return.
				//
				// This asked only "is there a ReturnStmt somewhere in the if
				// body", and a review defeated it seven ways with the whole
				// suite green — including making the entire boot policy gate
				// inert. The two that this block now closes:
				//
				//	if err := ...; err != nil && false { return err }
				//	if err := ...; err == nil { return err }
				//
				// Both have a direct return. Neither ever fires. The comment
				// here used to claim a return "buried under a second condition"
				// was caught; it was not — that is precisely the first shape.
				errName := boundErrName(stmt.Init)
				if errName == "" || !isNotNilCheck(stmt.Cond, errName) {
					return true
				}
				direct := false
				for _, s := range stmt.Body.List {
					ret, isReturn := s.(*ast.ReturnStmt)
					if !isReturn {
						continue
					}
					// And it must return the error it just tested. A return of
					// nil, or of some unrelated value, ends the function
					// reporting success on a refusal.
					for _, res := range ret.Results {
						if mentionsIdent(res, errName) {
							direct = true
						}
					}
				}
				if !direct {
					return true
				}
				ast.Inspect(stmt.Init, func(m ast.Node) bool {
					if trackedCall(m, wantReturned) != "" {
						obeyed[m] = true
					}
					return true
				})
			case *ast.ReturnStmt:
				for _, res := range stmt.Results {
					ast.Inspect(res, func(m ast.Node) bool {
						if trackedCall(m, wantReturned) != "" {
							obeyed[m] = true
						}
						return true
					})
				}
			}
			return true
		})
	}

	// Now every call site of a tracked name, obeyed or not.
	obeyingSites := map[string]int{}
	for name := range funcs {
		if !seen[name] {
			continue
		}
		fnName := name
		ast.Inspect(funcs[name].Body, func(n ast.Node) bool {
			called := trackedCall(n, wantReturned)
			if called == "" {
				return true
			}
			if obeyed[n] {
				wantReturned[called] = true
				obeyingSites[called]++
				return true
			}
			t.Errorf("%s calls %s and discards the result. The check runs and its "+
				"answer is thrown away, so the server proceeds on a posture it "+
				"just established was unsafe. Every call site has to return on it, "+
				"not just one of them", fnName, called)
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

	// How MANY call sites obey, not merely whether one does.
	//
	// Every check above is satisfied by a single surviving occurrence, so
	// deleting one of two call sites passed. A review deleted the entire boot
	// block, and separately replaced the reload hook body with `return nil`,
	// and the suite stayed green each time: the other site kept the name-level
	// entry satisfied. The comment claiming "Appears TWICE … Both must return"
	// described an assertion that did not exist.
	//
	// Two sites, and they are not interchangeable. Boot alone misses the case
	// the check exists for — a checkpoint that adds `partition by` to an
	// existing entity emits no DDL and arrives at a RUNNING server — and the
	// reload hook alone never examines the schema the server started with.
	for name, atLeast := range map[string]int{
		// trackedCall reports the bare selector, not the qualified name.
		"partitionGate": 2,
	} {
		if obeyingSites[name] >= atLeast {
			continue
		}
		t.Errorf("%s is returned on at %d call site(s); it must be at least %d. "+
			"One is the boot check and one is the hot-reload hook, and neither "+
			"covers the other: a `partition by` added to an existing entity emits "+
			"no migration and reaches a running server, and a server that boots "+
			"clean can be handed an unsafe checkpoint a second later. Deleting "+
			"either leaves ATL_REQUIRE_TENANT_ISOLATION gating half of what it "+
			"claims", name, obeyingSites[name], atLeast)
	}

	if !wantLocal["partitionGate"] {
		t.Error("cmd/server's run() never reaches partitionGate, so nothing " +
			"decides whether a schema declaring `partition by` may be served. " +
			"The decision itself is tested in TestPartitionGate; what this " +
			"asserts is only that it is wired in")
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
	for _, name := range []string{
		"interceptors.NewPartition", "interceptors.NewPartitionStream",
		"pg.VerifyPartitionPolicies",
		"dynServer.SetOnReload",
	} {
		if want[name] {
			continue
		}
		t.Errorf("cmd/server's run() never reaches %s. Depending on which one "+
			"this is: no request carries the caller's tenant and every "+
			"`partition by` entity refuses everything, or nothing checks that "+
			"the tables those entities name actually carry an enforced policy "+
			"— which is the case where the server looks healthy and returns "+
			"every tenant's rows", name)
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

// trackedCall returns the tracked name n calls, or "".
//
// Handles both spellings, because the boot decisions are a mix: two are
// package-local identifiers and one is pg.PartitionPolicyError. Matching only
// bare identifiers meant the tracked entry for the qualified one could never be
// satisfied, so discarding its result passed.
func trackedCall(n ast.Node, tracked map[string]bool) string {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if _, ok := tracked[fun.Name]; ok {
			return fun.Name
		}
	case *ast.SelectorExpr:
		if _, ok := tracked[fun.Sel.Name]; ok {
			return fun.Sel.Name
		}
	}
	return ""
}

// A probe that could not run must not read as a clean bill of health.
//
// The role check has enforced this since it was extracted; the policy check did
// not, and a review found both its boot and reload paths logging a warning and
// continuing on a catalog error — under the very flag whose purpose is to
// refuse. A locked-down pg_class, a statement timeout or a name mismatch each
// produce that error.
//
// Scoped to schemas that have something to protect: a deployment with no
// partitioned entity gains nothing from a failed probe stopping its start.
func TestPartitionedEntities(t *testing.T) {
	part := dsl.Entity{Name: "Doc", Namespace: "pt", PartitionField: "tenant"}
	plain := dsl.Entity{Name: "Plain", Namespace: "pt"}

	for _, tc := range []struct {
		name string
		ir   *dsl.IR
		want int
	}{
		{"nil IR", nil, 0},
		{"no entities", &dsl.IR{}, 0},
		{"only unpartitioned", &dsl.IR{Entities: []dsl.Entity{plain}}, 0},
		{"one partitioned", &dsl.IR{Entities: []dsl.Entity{plain, part}}, 1},
		{"two partitioned", &dsl.IR{Entities: []dsl.Entity{part, part}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(partitionedEntities(tc.ir)); got != tc.want {
				t.Errorf("counted %d partitioned entities, want %d. This decides "+
					"whether a failed catalog probe stops the server: too low and "+
					"the guard is skipped on a schema that needs it", got, tc.want)
			}
		})
	}
}

// boundErrName returns the single name an `if x := f(); ...` init binds.
func boundErrName(init ast.Stmt) string {
	assign, ok := init.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) == 0 {
		return ""
	}
	// The error is conventionally last: `problems, err := f()`.
	id, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok || id.Name == "_" {
		return ""
	}
	return id.Name
}

// isNotNilCheck reports whether cond is exactly `name != nil`.
//
// Exactly, deliberately. `err != nil && <anything>` is rejected, because the
// extra conjunct is the shape that made the refusal unreachable while looking
// correct. A legitimate call site that genuinely needs a compound condition
// will fail this test, and that is the right trade: it is a two-line change to
// hoist the extra condition out, and the alternative is a check that cannot
// tell a guard from a disabled guard.
func isNotNilCheck(cond ast.Expr, name string) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	lhs, ok := bin.X.(*ast.Ident)
	if !ok || lhs.Name != name {
		return false
	}
	rhs, ok := bin.Y.(*ast.Ident)
	return ok && rhs.Name == "nil"
}

// mentionsIdent reports whether expr references name anywhere.
func mentionsIdent(expr ast.Expr, name string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// The boot gate, tested by behaviour rather than by reading main.go's AST.
//
// # Why this replaces a source-level test
//
// The AST test in this file proves a call site exists. It cannot prove the call
// works, and a review demonstrated that at length: seven mutations made the
// whole gate inert with the entire suite green — a second condition that can
// never hold, an inverted condition, a return of nil, and deleting either call
// site outright. Four rounds of hardening each closed the escape just used and
// left the next one open.
//
// The decision now lives in one pure function, so every branch is reachable
// here. What the AST test still owns is narrow and appropriate: is that
// function reached from BOTH the boot path and the reload hook.
// KNOWN LIMIT, stated rather than papered over.
//
// This proves the decision. The AST test below proves the call sites exist and
// return. Neither checks the ARGUMENTS, and a review confirmed six mutations
// that survive because of it — replacing cfg.RequireTenantIsolation with false,
// policyProblems with nil, or perr with nil at either call site makes the gate
// inert with the whole suite green.
//
// A fifth AST rule would have closed some and left the rest; that loop had run
// four times, each round hardening against the escape just used. What closed it
// instead is TestServerRefusesToBootWhenAPartitionedTableHasNoPolicy in
// boot_pg_test.go, which boots run() against a real database as a non-superuser
// role and asserts whether the process starts. Arguments are not observable in
// source and are unavoidable in behaviour.
//
// The reload hook's call site is now covered the same way, by
// TestReloadRefusesASchemaTheDatabaseIsNotEnforcing in reload_pg_test.go: a
// server boots clean, the checkpoint changes underneath it to declare
// `partition by` on a table with no policy, and the test asserts on which of
// the two outcomes the running server logs. It drives the accepted case too,
// so a gate that refused every reload would not pass it.
//
// `perr`, the probe-failure path, was the last one open and is now closed at
// both call sites — TestServerRefusesToBootWhenThePolicyProbeCannotRun and
// TestReloadRefusesWhenThePolicyProbeCannotRun. Both revoke SELECT on
// pg_catalog.pg_policy in their own throwaway database, so the role keeps
// answering pg_class and errors on pg_policy: the shape a locked-down catalogue
// actually has, rather than a dead connection that fails everything and would
// prove nothing about this branch. Each drives the accepted case with the same
// fixture, so a gate that refused unconditionally would not pass either.
//
// All six of the argument mutations named above are now dead. Nothing about the
// gate is covered by source-reading alone.
func TestPartitionGate(t *testing.T) {
	probeFailed := errors.New("permission denied for table pg_policy")
	findings := []string{"shop.Doc declares `partition by` but has no policy"}

	for _, tc := range []struct {
		name        string
		problems    []string
		probeErr    error
		require     bool
		partitioned int
		wantErr     bool
		why         string
	}{
		{
			name: "clean schema starts", require: true, partitioned: 1,
			wantErr: false,
			why:     "a schema whose tables carry the policy must serve",
		},
		{
			name:     "findings refuse under the flag",
			problems: findings, require: true, partitioned: 1, wantErr: true,
			why: "this is the whole purpose of ATL_REQUIRE_TENANT_ISOLATION",
		},
		{
			name:     "findings warn without the flag",
			problems: findings, require: false, partitioned: 1, wantErr: false,
			why: "an operator must be able to see the problem before making it fatal",
		},
		{
			// A check that could not run is not a check that passed.
			name:     "a failed probe refuses under the flag",
			probeErr: probeFailed, require: true, partitioned: 1, wantErr: true,
			why: "a locked-down pg_class or a statement timeout would otherwise " +
				"let the server boot with the guard nominally on and never asked",
		},
		{
			name:     "a failed probe warns without the flag",
			probeErr: probeFailed, require: false, partitioned: 1, wantErr: false,
			why: "without the flag nothing is fatal",
		},
		{
			name:     "a failed probe with no partitioned entities starts",
			probeErr: probeFailed, require: true, partitioned: 0, wantErr: false,
			why: "there is nothing to enforce, so a briefly unreadable database " +
				"must not stop a deployment that declares no partition",
		},
		{
			// The probe error wins: its findings are not trustworthy.
			name:     "a failed probe refuses even with an empty problem list",
			problems: nil, probeErr: probeFailed, require: true, partitioned: 2,
			wantErr: true,
			why:     "an empty list from a failed probe means unknown, not clean",
		},
		{
			// Precedence, which no case exercised. The comment on the case
			// above claimed to test it and did not: its problems list was nil,
			// so it was input-identical to the case before it. Two mutations
			// lived in that gap — swapping the two checks, and reporting only
			// the first of several findings.
			name:     "a failed probe AND findings refuses, naming the probe",
			problems: findings, probeErr: probeFailed, require: true, partitioned: 1,
			wantErr: true,
			why:     "findings gathered by a probe that failed are not trustworthy",
		},
		{
			// More than one finding. A single-element list left every
			// multi-finding path untested, including whether any are dropped.
			name: "several findings all refuse and are all reported",
			problems: []string{
				"shop.Doc declares partition by but has no policy",
				"shop.Line declares partition by but has no policy",
			},
			require: true, partitioned: 2, wantErr: true,
			why: "a plan is refused as a unit; reporting one of several sends the " +
				"operator round the loop once per entity",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := partitionGate(tc.problems, tc.probeErr, tc.require, tc.partitioned)
			if tc.wantErr && err == nil {
				t.Fatalf("served the schema and should have refused. %s", tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("refused with %v and should have served. %s", err, tc.why)
			}
			// The message must carry the finding text, or an operator learns
			// only that something is wrong. A gate returning the probe error
			// bare, or only the first of several findings, passed every
			// assertion here until this was added.
			if err != nil && tc.probeErr == nil {
				for _, p := range tc.problems {
					if !strings.Contains(err.Error(), p) {
						t.Errorf("the refusal drops the finding %q, so the operator "+
							"cannot see which entity is unprotected: %v", p, err)
					}
				}
			}
			// A refusal has to say which condition fired, or an operator cannot
			// act on it.
			if tc.wantErr && tc.probeErr != nil && !errors.Is(err, probeFailed) {
				t.Errorf("the refusal does not wrap the probe error, so the "+
					"operator cannot see WHY the check could not run: %v", err)
			}
		})
	}
}
