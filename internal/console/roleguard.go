package console

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// consoleRoleError reports whether the console's database role leaves its
// row-level-security boundary ineffective, in which case startup must stop.
//
// A SUPERUSER or a role holding BYPASSRLS reads through the RESTRICTIVE policy:
// it stays attached, `\d` still lists it, and every query returns every
// organisation's rows. Unconditionally fatal, unlike pg.RequireIsolatedRole.
//
// A function so a test can call it; derr is DetectRolePrivileges' error.
func consoleRoleError(privs pg.RolePrivileges, derr error) error {
	if derr != nil {
		// A locked-down pg_roles, or a pooler rewriting current_user, would
		// otherwise start the console with the guard nominally present and
		// never asked.
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
