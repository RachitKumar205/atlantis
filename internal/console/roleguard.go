package console

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// consoleRoleError decides whether the console's database posture must stop
// startup.
//
// # Why the console needs this at all
//
// The console's own tables carry per-organisation data, and the boundary
// between organisations is a RESTRICTIVE row-level-security policy. A policy is
// only a boundary for a role the database applies it to: a SUPERUSER, or any
// role holding BYPASSRLS, reads straight through it. The policy is still
// attached, `\d` still lists it, and every query returns every organisation's
// rows.
//
// That is the same failure pg.RequireIsolatedRole refuses for the server, and
// the console reaches it by a different door — CONSOLE_PG_URL is a separate
// setting, so a deployment can get the server's role right and the console's
// wrong while every check reports healthy.
//
// # Why it is fatal rather than a flag
//
// The server's equivalent is opt-in behind ATL_REQUIRE_TENANT_ISOLATION,
// because a single-tenant deployment predates the feature and defaulting to
// fatal would break installs that never used it. The console has no such
// history: it is becoming multi-organisation, and there is no version of it
// where reading another organisation's audit log is acceptable.
//
// A flag here would also be a guard that depends on someone remembering to turn
// it on — and the moment it matters is the moment forgetting produces a
// cross-organisation read rather than an error.
//
// # Why this is a function rather than four lines in New
//
// Copied deliberately from cmd/server/tenantisolation.go, whose comment records
// what happens otherwise: a boot check written inline is not reachable from a
// test, because whether it ran is only observable by booting against a
// database, and a boot test does not fail when the refusal is deleted — the
// server simply starts, which is what it does when the check passes. Every
// mutation of the inline version survived the suite.
//
// derr is DetectRolePrivileges' error, carried in rather than handled at the
// call site: "could not establish the answer" is one of the outcomes this has
// to rank, not a separate concern.
func consoleRoleError(privs pg.RolePrivileges, derr error) error {
	if derr != nil {
		// A check that could not run is not a check that passed. A locked-down
		// pg_roles, or a pooler rewriting current_user, would otherwise start
		// the console with the guard nominally present and never actually
		// asked.
		return fmt.Errorf("refusing to start: could not determine whether the "+
			"console's database role enforces row-level security: %w", derr)
	}
	if err := pg.RequireIsolatedRole(privs); err != nil {
		return fmt.Errorf("refusing to start: %w\n\n"+
			"The console stores per-organisation data and separates it with "+
			"row-level security, which this role reads straight through.\n\n"+
			"Point CONSOLE_PG_URL at a role that is NOSUPERUSER and "+
			"NOBYPASSRLS. Locally: make dev-console-role", err)
	}
	return nil
}
