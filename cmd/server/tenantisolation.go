package main

import (
	"errors"
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// tenantIsolationError decides whether the database posture must stop startup.
//
// The whole decision lives in one pure function rather than inline in run()
// because it is the only part of the check that can be tested. Whether a boot
// check ran is not observable without booting against a database, and a boot
// test would not fail if the refusal were deleted — the server would simply
// start, which is what it does when the check passes. Inline, every mutation of
// this logic survived the suite: making the flag inert in either branch left
// both packages green.
//
// derr is DetectRolePrivileges' error, carried in rather than handled at the
// call site, because "could not establish the answer" is one of the three
// outcomes this has to rank and not a separate concern.
func tenantIsolationError(privs pg.RolePrivileges, derr error, require bool) error {
	if !require {
		// Opt-in, matching the Timescale check. A deployment with no
		// partitioned entities is unaffected either way, so defaulting to fatal
		// would break every existing single-tenant install to protect a feature
		// it does not use.
		return nil
	}
	if derr != nil {
		// A check that could not run is not a check that passed. Failing to
		// establish the answer has to be as fatal as establishing a bad one —
		// otherwise a locked-down pg_roles or a pooler rewriting current_user
		// starts the server with the guard nominally on and never actually
		// asked.
		return fmt.Errorf("refusing to start: could not determine whether the "+
			"database role enforces row-level security, and "+
			"ATL_REQUIRE_TENANT_ISOLATION is set: %w", derr)
	}
	if err := pg.RequireIsolatedRole(privs); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	return nil
}

// storedSQLAuditError decides whether findings from the stored-schema audit
// must stop startup.
//
// A pure function for the same reason tenantIsolationError is one: inline, the
// flag's effect is unreachable from a test, and a mutation making it inert
// survived the whole suite. The audit itself has tests; whether its findings
// mean anything did not.
func storedSQLAuditError(findings []error, require bool) error {
	if len(findings) == 0 || !require {
		return nil
	}
	return fmt.Errorf("refusing to start: the stored schema contains SQL that can "+
		"rebind the caller's tenant, and ATL_REQUIRE_TENANT_ISOLATION is set: %w",
		errors.Join(findings...))
}

// partitionedEntities returns the ids of every entity declaring `partition by`.
//
// The boot and reload policy checks refuse when they cannot run, but only on a
// schema that has something to protect: a deployment with no partitioned entity
// gains nothing from a failed catalog probe stopping its start, and defaulting
// to fatal there would break installs that use no such entity.
func partitionedEntities(ir *dsl.IR) []string {
	if ir == nil {
		return nil
	}
	var out []string
	for i := range ir.Entities {
		if ir.Entities[i].PartitionField != "" {
			out = append(out, ir.Entities[i].ID())
		}
	}
	return out
}

// partitionGate answers the one question the boot check and the hot-reload hook
// both ask: may this schema be served?
//
// # Why this is a function and not two inline blocks
//
// It was two inline blocks, and they were verified by a test that read main.go's
// AST — it looked for the call, then for a return, then for the condition, then
// for the returned value, and a review defeated it at every stage. Seven
// mutations made the whole gate inert with the suite green, including deleting
// the boot block outright. Each round of hardening was aimed at the escape the
// previous round had used.
//
// An AST test proves a call site EXISTS. It cannot prove the call WORKS, and
// four attempts to make it do so produced a test nobody could reason about. So
// the decision moved here, where it is a pure function of its inputs and every
// branch is reachable from a test that asserts behaviour. What is left for the
// source-level test is only "is this reached from both places", which is the
// one thing an AST test is actually good at.
//
// # The rules
//
// A probe that FAILED is not a probe that passed. A locked-down pg_class, a
// statement timeout or a name mismatch would otherwise let the server boot with
// the guard nominally on and never actually asked. Under the flag, with at
// least one partitioned entity, that refuses.
//
// With no partitioned entities there is nothing to enforce, so a probe failure
// is a warning even under the flag — otherwise a deployment that declares no
// partition at all cannot start when the database is briefly unreadable.
//
// Findings themselves refuse only under the flag. Without it they are warnings,
// which is what lets an operator see the problem before making it fatal.
func partitionGate(problems []string, probeErr error, require bool, partitioned int) error {
	if probeErr != nil {
		if require && partitioned > 0 {
			return fmt.Errorf("could not determine whether partitioned entities "+
				"carry an enforced row-level security policy, and "+
				"ATL_REQUIRE_TENANT_ISOLATION is set: %w", probeErr)
		}
		// Nothing to enforce, or the operator has not asked for enforcement.
		// The caller logs; returning nil here means "serve it".
		return nil
	}
	return pg.PartitionPolicyError(problems, require)
}
