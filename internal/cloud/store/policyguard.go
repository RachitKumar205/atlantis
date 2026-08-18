package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// catalogQuerier is the slice of a pool this check needs. Narrow on purpose, so
// the check runs against a pool, a connection or a transaction — which is what
// lets a test run it under SET LOCAL ROLE, and the role is half of what this
// verifies.
type catalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// unpolicedTables are the Cloud tables that deliberately carry no per-user
// boundary.
//
// Every entry is a decision, and it is here rather than in a comment so that
// adding a table without deciding is impossible — see VerifyPolicies. The four
// below are argued in full in migrations/cloud/0001; the one-liners are
// reminders, not the reasoning.
//
// # Why this list is currently everything
//
// Because none of Cloud's first four tables can carry the boundary. Two of them
// are the queries that *discover* who a request is, so they cannot be filtered
// by who the request is; one is a registry; one is read from both directions.
// That makes this guard, today, a tripwire rather than an enforcement — and the
// tripwire is the point. The second-factor secrets arriving next are the first
// tables here that are only ever read for an already-identified user, and this
// is what stops them landing unpoliced because nobody thought about it.
var unpolicedTables = map[string]string{
	"users":       "the sign-in lookup is what discovers who the request is",
	"identities":  "the OAuth callback lookup is the same, one step along: a policy here makes every sign-in create a duplicate account",
	"orgs":        "the registry of organisations",
	"memberships": "read per-user by a member and per-org by an admin; a policy on user_id breaks the second",
}

// VerifyPolicies asks the live catalogue whether every table in schema cloud is
// either policed or a recorded exemption.
//
// # Why the catalogue rather than the migrations
//
// Reading the migration files answers what was intended. The question is what
// the database ended up with, and the two differ in the direction that matters:
// a policy can be enabled without being FORCED, in which case the owning role —
// which is the role Cloud connects as — reads straight through it while `\d`
// still lists it. The console's equivalent was written after a review
// reproduced exactly that on the server side.
//
// # Why unknown tables are a fault
//
// The dangerous case is not a policy that was removed; it is a table that never
// had one. Somebody adds cloud.totp_secrets, nobody thinks about isolation, and
// it holds every user's second factor from its first row onward. Requiring
// every table to be either policed or named above turns that from an oversight
// into a boot failure.
//
// # Why the connecting role is checked here rather than at startup
//
// FORCE ROW LEVEL SECURITY subjects a table's owner to its policies. It does
// not subject a *superuser*, and it does not subject a role holding BYPASSRLS —
// either of those reads straight through every policy in the schema while `\d`
// keeps listing them.
//
// So the role is half of this check. It is folded in here rather than run as
// its own startup gate because of when it starts mattering: while every table
// is exempt, a bypassing role bypasses nothing, and refusing to start on one
// would be friction with no protection behind it. The moment a policed table
// exists the role decides whether that policy does anything, so the check arms
// itself exactly then — see the count below.
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
		// A check that could not run is not a check that passed.
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
	// Asked only when there is a policy, so this costs nothing until there is
	// something for it to protect — and arms itself the moment there is.
	if policed > 0 {
		privs, derr := pg.DetectRolePrivileges(ctx, pg.PgxRoleQuerier{Q: q})
		if derr != nil {
			// A check that could not run is not a check that passed. A
			// locked-down pg_roles, or a pooler rewriting current_user, would
			// otherwise leave the guard nominally present and never asked.
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
