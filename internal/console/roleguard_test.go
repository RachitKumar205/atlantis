package console

import (
	"errors"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// Every posture the console can boot into, ranked.
//
// The table exists because the interesting cases are the ones that must REFUSE,
// and a guard that refused everything would pass any test that only checked the
// refusals. The clean row is the other half.
func TestConsoleRoleError(t *testing.T) {
	cases := []struct {
		name    string
		privs   pg.RolePrivileges
		derr    error
		wantErr bool
		// why records what a reader should understand from the case, and is
		// printed on failure so a broken guard says what it broke.
		why string
	}{
		{
			name:    "superuser",
			privs:   pg.RolePrivileges{Name: "atlantis", Superuser: true},
			wantErr: true,
			why: "a superuser reads through RESTRICTIVE policies, so every " +
				"organisation's rows are visible to every request",
		},
		{
			name:    "bypassrls",
			privs:   pg.RolePrivileges{Name: "atlantis", BypassRLS: true},
			wantErr: true,
			why: "BYPASSRLS skips row-level security regardless of FORCE, so " +
				"the org boundary is attached and inert",
		},
		{
			name:    "both",
			privs:   pg.RolePrivileges{Name: "atlantis", Superuser: true, BypassRLS: true},
			wantErr: true,
			why:     "neither attribute rescues the other",
		},
		{
			name:    "detection failed",
			privs:   pg.RolePrivileges{},
			derr:    errors.New("permission denied for table pg_roles"),
			wantErr: true,
			why: "a check that could not run is not a check that passed — " +
				"otherwise a locked-down pg_roles starts the console with the " +
				"guard present and never asked",
		},
		{
			name:    "clean role",
			privs:   pg.RolePrivileges{Name: "atlantis_console"},
			wantErr: false,
			why: "the ordinary case. Without this row a guard that refused " +
				"every role would pass every other case here, and the console " +
				"would never start at all",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := consoleRoleError(tc.privs, tc.derr)
			if tc.wantErr && err == nil {
				t.Fatalf("the console started on this role, but %s", tc.why)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("the console refused a role it should accept (%s): %v",
					tc.why, err)
			}
		})
	}
}

// The refusal has to say what to do about it.
//
// An operator meeting this has the error text and nothing else: the console
// exits before it serves anything, so there is no page to read and no log to
// browse.
func TestConsoleRoleErrorSaysHowToFixIt(t *testing.T) {
	err := consoleRoleError(pg.RolePrivileges{Name: "atlantis", Superuser: true}, nil)
	if err == nil {
		t.Fatal("no error for a superuser role")
	}
	msg := err.Error()
	for _, want := range []string{"NOSUPERUSER", "NOBYPASSRLS", "CONSOLE_PG_URL", "make dev-console-role"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so an operator hitting it "+
				"has to go read the source to find the fix:\n%s", want, msg)
		}
	}
}
