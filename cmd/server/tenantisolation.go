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
