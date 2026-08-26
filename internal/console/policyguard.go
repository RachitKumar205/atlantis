package console

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// rowQuerier is the slice of a pgx pool this check needs. Narrow, so the check
// is callable against a pool, a connection or a transaction, which is what lets
// a test run it under SET LOCAL ROLE.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// unpolicedTables are the console tables that carry no organisation boundary.
// verifyConsolePolicies fails on any table in the schema that is neither
// policed nor listed here, so a new table cannot arrive undecided.
//
//   - sessions: the bootstrap table. getSessionInfo is the query that
//     discovers which organisation a request belongs to, so it cannot be
//     filtered by that organisation. Its bulk operations are scoped through
//     orgStore instead, which makes writing one without an organisation a
//     compile error.
//
//   - spent_assertions: global by design. A replay check that is per
//     organisation is not a replay check — the same assertion could be spent
//     once in each. jti is 128 bits from crypto/rand, so the ids do not
//     collide across organisations by accident.
//
//   - orgs: the registry. A list of organisations that only its own
//     organisation could read would be useless, and it holds nothing that is
//     not already in every assertion.
//
//   - caller_certs: the same bootstrap shape as sessions, one layer out. It maps
//     a certificate's fingerprint to the organisation and caller it was issued
//     for, and renewal resolves the organisation from the certificate before
//     anything knows it, so it cannot be filtered by it. The key is a SHA-256
//     over the certificate, so a lookup requires already holding it. Handlers
//     that list these rows scope them by the session's organisation in Go.
//
// console.enroll_tokens is absent and is policed. The enrolment route has no
// session, so the request names the organisation, the handler binds it, and the
// RESTRICTIVE policy compares it against the row.
//
// console_schema_migrations is not listed because it is not in this schema:
// internal/migrate pins search_path=public, so golang-migrate's bookkeeping
// lands in public and the query below never sees it.
var unpolicedTables = map[string]string{
	"sessions":         "bootstrap: the session lookup is what discovers the organisation",
	"spent_assertions": "global by design: a per-organisation replay check is not a replay check",
	"orgs":             "the registry of organisations",
	"caller_certs":     "bootstrap: the fingerprint lookup is what discovers the organisation",
}

// verifyConsolePolicies asks the live catalogue whether the organisation
// boundary is in place. Binding succeeds whether or not a policy exists, and so
// does every query after it, so a console with no policy is indistinguishable
// from a working one.
//
// An unlisted table is an error: one that never had a policy holds several
// organisations' rows from its first insert.
func verifyConsolePolicies(ctx context.Context, q rowQuerier) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname,
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       COALESCE(
		           (SELECT count(*)
		              FROM pg_policy p
		             WHERE p.polrelid = c.oid
		               AND NOT p.polpermissive
		               AND pg_get_expr(p.polqual, p.polrelid) LIKE '%current_org%'), 0)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'console'
		   AND c.relkind IN ('r', 'p')
		   AND c.relispartition = false
		 ORDER BY c.relname`)
	if err != nil {
		return fmt.Errorf("could not verify the organisation boundary: %w", err)
	}
	defer rows.Close()

	var faults []string
	seen := 0
	for rows.Next() {
		var (
			name             string
			rls, forced      bool
			boundaryPolicies int
		)
		if err := rows.Scan(&name, &rls, &forced, &boundaryPolicies); err != nil {
			return fmt.Errorf("could not verify the organisation boundary: %w", err)
		}
		seen++

		if _, exempt := unpolicedTables[name]; exempt {
			continue
		}
		switch {
		case !rls:
			faults = append(faults, fmt.Sprintf("console.%s has no row-level security", name))
		case !forced:
			// ENABLE without FORCE leaves the owner exempt, and the console's
			// role owns these tables. `\d` lists the policy; it applies to
			// nobody who connects.
			faults = append(faults, fmt.Sprintf(
				"console.%s has row-level security enabled but not FORCED, so the owning role reads through it", name))
		case boundaryPolicies == 0:
			faults = append(faults, fmt.Sprintf(
				"console.%s has no RESTRICTIVE policy referencing console.current_org()", name))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("could not verify the organisation boundary: %w", err)
	}
	if seen == 0 {
		return fmt.Errorf("could not verify the organisation boundary: no tables found in schema console")
	}
	if len(faults) == 0 {
		return nil
	}

	sort.Strings(faults)
	var b strings.Builder
	b.WriteString("refusing to start: the organisation boundary is not in place.\n\n")
	for _, f := range faults {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	b.WriteString("\nThis console serves several organisations from one database, and " +
		"row-level security is what separates them. Without it every organisation " +
		"reads every other organisation's rows, and nothing an operator can observe " +
		"would show it — sign-in works, the pages render, the data is simply " +
		"everyone's.\n\n")
	b.WriteString("A table that should not be policed belongs in unpolicedTables " +
		"(internal/console/policyguard.go) with the reason, so the decision is " +
		"recorded rather than implied. Currently exempt:\n")
	exempt := make([]string, 0, len(unpolicedTables))
	for name, why := range unpolicedTables {
		exempt = append(exempt, fmt.Sprintf("  console.%s — %s", name, why))
	}
	sort.Strings(exempt)
	b.WriteString(strings.Join(exempt, "\n") + "\n")
	return fmt.Errorf("%s", b.String())
}
