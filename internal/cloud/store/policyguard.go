package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// catalogQuerier is satisfied by a pool, a connection or a transaction, so a
// test can run VerifyPolicies under SET LOCAL ROLE.
type catalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// unpolicedTables are the tables in schema cloud that carry no per-user
// boundary. VerifyPolicies fails on any table in the schema that is neither
// policed nor listed here, so a new table cannot arrive undecided.
//
// Each entry is a lookup that discovers who a request is, a registry, or a
// table read from both directions; none can be filtered by the current user,
// because no user is known until the lookup answers. migrations/cloud/0001
// carries the full reasoning.
var unpolicedTables = map[string]string{
	"users":          "the sign-in lookup is what discovers who the request is",
	"identities":     "the OAuth callback lookup is the same, one step along: a policy here makes every sign-in create a duplicate account",
	"orgs":           "the registry of organisations",
	"memberships":    "read per-user by a member and per-org by an admin; a policy on user_id breaks the second",
	"email_tokens":   "a reset link is spent by token hash with nobody signed in, so a policy keyed to the current user matches nothing and every reset reports an invalid token",
	"sessions":       "the bootstrap table: the session lookup is what discovers which user a request is",
	"pending_logins": "the same, one step earlier — a half-finished login is resolved by token before anybody is identified",

	// The two below are written by the provisioner, which claims work from a
	// queue rather than serving a request. A policy keyed to current_user_id
	// would match nothing and hide every row from the only process that reads
	// them.
	"org_provisioning": "a work queue claimed by a background process, which has no current user to be filtered by",
	"audit_log":        "the record of what that process did; scoped by organisation rather than by user, and read by operators looking at an organisation they may not belong to",
}

// VerifyPolicies asks the live catalogue whether every table in schema cloud is
// either policed or a recorded exemption.
//
// It reads the catalogue rather than the migrations, which record intent. A
// policy can be ENABLE'd without being FORCE'd, and the owning role — the role
// Cloud connects as — then reads through it while `\d` still lists it.
//
// The connecting role is checked too. FORCE ROW LEVEL SECURITY subjects a
// table's owner to its policies, but not a superuser and not a role holding
// BYPASSRLS; either reads through every policy in the schema. That check runs
// only once a policed table exists, since a bypassing role bypasses nothing
// while every table is exempt.
func VerifyPolicies(ctx context.Context, q catalogQuerier) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname,
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       COALESCE(
		           (SELECT count(*)
		              FROM pg_policy p
		             WHERE p.polrelid = c.oid
		               AND NOT p.polpermissive
		               AND pg_get_expr(p.polqual, p.polrelid) LIKE '%current_user_id%'), 0)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'cloud'
		   AND c.relkind IN ('r', 'p')
		   AND c.relispartition = false
		 ORDER BY c.relname`)
	if err != nil {
		return fmt.Errorf("could not verify the user boundary: %w", err)
	}
	defer rows.Close()

	var faults []string
	seen, policed := 0, 0
	for rows.Next() {
		var (
			name             string
			rls, forced      bool
			boundaryPolicies int
		)
		if err := rows.Scan(&name, &rls, &forced, &boundaryPolicies); err != nil {
			return fmt.Errorf("could not verify the user boundary: %w", err)
		}
		seen++

		if _, exempt := unpolicedTables[name]; exempt {
			continue
		}
		if rls && forced && boundaryPolicies > 0 {
			policed++
		}
		switch {
		case !rls:
			faults = append(faults, fmt.Sprintf("cloud.%s has no row-level security", name))
		case !forced:
			faults = append(faults, fmt.Sprintf(
				"cloud.%s has row-level security enabled but not FORCED, so the owning role reads through it", name))
		case boundaryPolicies == 0:
			faults = append(faults, fmt.Sprintf(
				"cloud.%s has no RESTRICTIVE policy referencing cloud.current_user_id()", name))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("could not verify the user boundary: %w", err)
	}
	if seen == 0 {
		return fmt.Errorf("could not verify the user boundary: no tables found in schema cloud")
	}

	// A policy only does something if the connecting role is subject to it.
	if policed > 0 {
		privs, derr := pg.DetectRolePrivileges(ctx, pg.PgxRoleQuerier{Q: q})
		if derr != nil {
			// A locked-down pg_roles, or a pooler rewriting current_user,
			// leaves the guard nominally present and never asked, so failing
			// to determine this is a failure to start.
			return fmt.Errorf("could not determine whether Cloud's database role "+
				"enforces row-level security: %w", derr)
		}
		if err := pg.RequireIsolatedRole(privs); err != nil {
			faults = append(faults, fmt.Sprintf(
				"%d table(s) carry a user boundary, and the connecting role reads "+
					"straight through it: %v", policed, err))
		}
	}

	if len(faults) == 0 {
		return nil
	}

	sort.Strings(faults)
	var b strings.Builder
	b.WriteString("refusing to start: a Cloud table is neither policed nor a recorded exemption.\n\n")
	for _, f := range faults {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	b.WriteString("\nOne Cloud process authenticates everybody, so a table holding per-user " +
		"rows with no boundary is one forgotten WHERE clause away from handing one " +
		"person another person's account — and nothing an operator can observe would " +
		"show it, because sign-in works and the pages render either way.\n\n")
	b.WriteString("A table that should not be policed belongs in unpolicedTables " +
		"(internal/cloud/store/policyguard.go) with the reason, so the decision is " +
		"recorded rather than implied. Currently exempt:\n")
	exempt := make([]string, 0, len(unpolicedTables))
	for name, why := range unpolicedTables {
		exempt = append(exempt, fmt.Sprintf("  cloud.%s — %s", name, why))
	}
	sort.Strings(exempt)
	b.WriteString(strings.Join(exempt, "\n") + "\n")
	return fmt.Errorf("%s", b.String())
}
