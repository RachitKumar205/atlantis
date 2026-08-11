package pg

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// PartitionedTable names one table an entity declaring `partition by` maps to.
type PartitionedTable struct {
	EntityID string
	Schema   string
	Table    string
	// Column is the discriminator the policy must compare. Empty skips the
	// predicate check and leaves only the catalog flags.
	Column string
}

// VerifyPartitionPolicies reports every partitioned entity whose table does not
// actually carry an enforced row-level security policy.
//
// # Why this is needed at all
//
// The policy, FORCE ROW LEVEL SECURITY and the backing index are emitted on
// CREATE TABLE only. No differ reads `partition by` (task #36), so adding the
// clause to an entity that already exists produces an empty plan: no migration,
// no policy, no output. The schema then claims a partition the database has
// never heard of.
//
// That was survivable while the clause was inert in both directions, and the
// docs said so. It stopped being survivable once the server began binding the
// tenant per request, because the only signal an operator can observe — omit
// the tenant, get refused — now works whether or not a policy exists. A review
// executed exactly that: the differ emitted nothing, the server refused an
// unbound request, and a bound request returned every tenant's rows. The
// appearance of isolation is worse than its absence, because nothing prompts
// anyone to look.
//
// So the boot check asks the database rather than the schema. relrowsecurity
// and relforcerowsecurity are catalog facts; a policy that was never created
// cannot answer yes.
//
// # Why FORCE, and not just ENABLE
//
// ENABLE alone exempts the table's owner, and atlantis owns the tables it
// creates. A table with ENABLE and without FORCE is a table whose policy does
// not apply to the one role that connects.
func VerifyPartitionPolicies(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error)
}, tables []PartitionedTable) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}

	// One round trip, not one per entity: this runs on the boot path.
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Schema+"."+t.Table)
	}

	// The policy's PREDICATES — plural, and every one of them.
	//
	// Counting rows in pg_policy answers "is there a policy", which is not the
	// question. Three shapes pass a count and isolate nothing: USING (true); a
	// predicate on the wrong column, reachable because changing the `partition
	// by` field is another differ gap that leaves the old policy in place; and a
	// FOR INSERT policy with no SELECT policy at all.
	//
	// Reading one aggregated string per table was the first repair, and it was
	// two bugs. It concatenated the predicate and the command with a `|`, which
	// is a legal character in a predicate — `(a || b)`, `body ~ 'x|y'`,
	// `(flags | 4)` — so the split took the first one and every such table was
	// reported broken. That refuses boot AND every hot reload, permanently, on a
	// healthy deployment, and it bites exactly the hand-extended policies this
	// check exists to watch. And it accepted a table as soon as ANY policy
	// matched, so a correct policy sitting beside `USING (true)` passed while
	// the pair — permissive policies are OR'd — returned every tenant's rows.
	//
	// One row per policy, no delimiter, and the question asked of all of them.
	rows, err := q.Query(ctx, `
SELECT n.nspname || '.' || c.relname, c.relrowsecurity, c.relforcerowsecurity,
       coalesce(pg_get_expr(p.polqual, p.polrelid), ''),
       coalesce(pg_get_expr(p.polwithcheck, p.polrelid), ''),
       coalesce(p.polcmd::text, ''), coalesce(p.polpermissive, false),
       (p.oid IS NOT NULL)
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_policy p ON p.polrelid = c.oid
 WHERE n.nspname || '.' || c.relname = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("pg: read partition policies: %w", err)
	}
	defer rows.Close()

	type policy struct {
		qual, withCheck, cmd string
		permissive           bool
	}
	type state struct {
		enabled, forced bool
		policies        []policy
	}
	live := map[string]state{}
	for rows.Next() {
		var qualified string
		var pol policy
		var enabled, forced, present bool
		if err := rows.Scan(&qualified, &enabled, &forced,
			&pol.qual, &pol.withCheck, &pol.cmd, &pol.permissive, &present); err != nil {
			return nil, fmt.Errorf("pg: scan partition policy: %w", err)
		}
		st := live[qualified]
		st.enabled, st.forced = enabled, forced
		if present {
			st.policies = append(st.policies, pol)
		}
		live[qualified] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate partition policies: %w", err)
	}

	var problems []string
	for _, t := range tables {
		qualified := t.Schema + "." + t.Table
		st, present := live[qualified]

		// Read and write are asked separately because they are enforced
		// separately. USING governs which rows are visible; WITH CHECK governs
		// which rows may be written. A policy carrying a correct USING and
		// `WITH CHECK (true)` reports healthy to any check that reads only
		// polqual, and lets a request bound to one tenant INSERT a row stamped
		// with another — executed, not theorised. The emitter writes both, so a
		// table missing one has diverged from it.
		//
		// For ALL and UPDATE a null WITH CHECK means PostgreSQL reuses USING as
		// the check, so an empty withCheck falls back rather than counting as a
		// missing clause. DELETE has no check half; INSERT has no using half.
		readable, writable := 0, 0
		var unscopedRead, unscopedWrite []string
		for _, pol := range st.policies {
			if !pol.permissive {
				// RESTRICTIVE policies AND with the permissive set; they can
				// only narrow. They cannot supply the isolation, and a table
				// with no permissive policy denies everything.
				continue
			}
			if governsRead(pol.cmd) {
				readable++
				if t.Column != "" && !predicateScopesColumn(pol.qual, t.Column) {
					unscopedRead = append(unscopedRead, pol.qual)
				}
			}
			if governsWrite(pol.cmd) {
				writable++
				check := pol.withCheck
				if check == "" {
					check = pol.qual
				}
				if t.Column != "" && !predicateScopesColumn(check, t.Column) {
					unscopedWrite = append(unscopedWrite, check)
				}
			}
		}

		switch {
		case !present:
			// A missing table IS a finding, not a skip.
			//
			// These entities come from the IR checkpoint, which only
			// ApplyMigration writes — so an entity that is in it has been
			// applied and its table exists. Absence therefore means either the
			// table was dropped underneath the schema, or this code computed a
			// name the database does not use.
			//
			// It was a `continue` first, on the reasoning that a fresh
			// deployment should not look broken. That turned every
			// name-computation mistake into a silent pass, and there was one:
			// the caller was building the name with TableName instead of
			// EntityPhysicalTable, so every entity using `table "..."` missed
			// its lookup and was reported healthy.
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and no table %s exists. Either it was "+
					"dropped, or atlantis is looking up the wrong name — and a "+
					"lookup that misses cannot tell you the policy is absent",
				t.EntityID, qualified))
		case len(st.policies) == 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` but %s carries no row-level security "+
					"policy. Every read returns every tenant's rows. The clause was "+
					"almost certainly added to an entity that already existed: the "+
					"differ does not read it, so `tide apply` emitted nothing",
				t.EntityID, qualified))
		case !st.enabled:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and %s has a policy, but row-level "+
					"security is disabled on the table, so the policy is inert",
				t.EntityID, qualified))
		case !st.forced:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and %s has row-level security enabled "+
					"without FORCE. The table's owner is exempt, and atlantis owns "+
					"the tables it creates, so the policy does not apply to the role "+
					"that connects",
				t.EntityID, qualified))
		case readable == 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and %s has policies, none of them "+
					"permissive and governing reads. A table whose only policies are "+
					"FOR INSERT, or are all RESTRICTIVE, denies every read rather "+
					"than scoping it",
				t.EntityID, qualified))
		case len(unscopedRead) > 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by %s` and %s has a permissive read policy "+
					"that does not compare that column to atlantis.current_partition(). "+
					"Permissive policies are OR'd, so ONE such policy beside a correct "+
					"one returns every tenant's rows — a check that accepted any "+
					"matching policy reported exactly that table healthy. Unscoped: %q",
				t.EntityID, t.Column, qualified, unscopedRead))
		case writable == 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and %s has no permissive policy governing "+
					"writes, so every INSERT and UPDATE is refused",
				t.EntityID, qualified))
		case len(unscopedWrite) > 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by %s` and %s has a permissive write policy "+
					"whose WITH CHECK does not compare that column to "+
					"atlantis.current_partition(). Reads are scoped and writes are not: "+
					"a request bound to one tenant can write a row stamped with "+
					"another. Unscoped: %q",
				t.EntityID, t.Column, qualified, unscopedWrite))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// PartitionPolicyError turns findings into the error that stops startup, or nil.
//
// Separated from the query for the same reason the other boot decisions are:
// whether a check refuses is the part a test can reach, and inline it was
// unreachable.
func PartitionPolicyError(problems []string, require bool) error {
	if len(problems) == 0 || !require {
		return nil
	}
	// No "refusing to start" here. This is also reached from the hot-reload
	// hook, where the server is already running and telling its operator it
	// refuses to start is simply false. The caller supplies that framing.
	return fmt.Errorf("%d partitioned entit%s have no enforced row-level "+
		"security, and ATL_REQUIRE_TENANT_ISOLATION is set:\n  %s",
		len(problems), plural(len(problems)), strings.Join(problems, "\n  "))
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// governsRead and governsWrite classify a policy's command.
//
// polcmd is '*' for ALL, 'r' SELECT, 'a' INSERT, 'w' UPDATE, 'd' DELETE.
// UPDATE and DELETE have a USING half, so they govern which rows are reachable;
// UPDATE and INSERT have a WITH CHECK half, so they govern what may be written.
// ALL is both.
func governsRead(cmd string) bool {
	return cmd == "*" || cmd == "r" || cmd == "w" || cmd == "d"
}

func governsWrite(cmd string) bool {
	return cmd == "*" || cmd == "a" || cmd == "w"
}

// predicateScopesColumn reports whether one policy predicate compares the
// discriminator column to the discriminator function.
//
// A substring test, deliberately: the emitted predicate is
// `<col> = atlantis.current_partition()[::cast]`, and re-parsing it to compare
// trees would be a second implementation of the emitter that could disagree
// with it. What this has to catch is the coarse failure — a predicate that
// names neither the column nor the function — and for that, presence is enough.
//
// It takes ONE predicate, not an aggregate. Packing several into a string and
// splitting them here is what produced both the `|`-in-a-predicate false
// negative and the any-match false positive; the caller now holds the list and
// asks about each member.
func predicateScopesColumn(qual, column string) bool {
	if !strings.Contains(qual, "current_partition") {
		return false
	}
	return containsIdentifier(qual, column)
}

// containsIdentifier reports whether qual references column as a whole
// identifier rather than as a fragment of a longer name.
//
// A plain substring test accepted a policy on the WRONG column whenever the
// declared name happened to appear inside the predicate text. Measured:
// `(other = atlantis.current_partition())` was reported as scoping the column
// `t`, `at`, and `partition` — the last because the name occurs inside
// `atlantis.current_partition()` itself. `(tenant_backup = ...)` was reported
// as scoping `tenant`.
//
// Identifier characters in PostgreSQL are letters, digits, underscore and
// dollar; a match bounded by anything else is a whole reference.
func containsIdentifier(qual, column string) bool {
	if column == "" {
		return false
	}
	isIdentChar := func(b byte) bool {
		return b == '_' || b == '$' ||
			(b >= '0' && b <= '9') ||
			(b >= 'a' && b <= 'z') ||
			(b >= 'A' && b <= 'Z')
	}
	for i := 0; i+len(column) <= len(qual); i++ {
		if qual[i:i+len(column)] != column {
			continue
		}
		if i > 0 && isIdentChar(qual[i-1]) {
			continue
		}
		if j := i + len(column); j < len(qual) && isIdentChar(qual[j]) {
			continue
		}
		// A qualified reference like `atlantis.current_partition` is not a
		// column mention, whatever it contains.
		if i > 0 && qual[i-1] == '.' {
			continue
		}
		return true
	}
	return false
}

// RLSBlockedTables reports which of the named tables would hide their rows from
// an unbound statement issued by the CURRENT role.
//
// # Why this asks the database rather than the schema
//
// The caller wants to know whether a maintenance statement that binds no tenant
// will silently match nothing. That is a property of the TABLE and the ROLE, not
// of the schema text, and a first version of the backfill guard got this wrong
// in three ways at once — all three executed against a live database:
//
//   - `partition by` declared, no policy on the table. This is the MODAL case,
//     because no differ emits the policy when the clause is added to an entity
//     that already exists — the gap this whole feature exists to detect. The
//     backfill works fine there, and the guard refused it.
//   - a role holding BYPASSRLS or superuser. pg.RequireIsolatedRole rejects that
//     posture only under ATL_REQUIRE_TENANT_ISOLATION, so it is a supported
//     deployment, and the policy simply does not apply to it.
//   - the mirror case, missed entirely: an entity that DROPPED `partition by`
//     keeps its policy, because removal emits nothing either. The schema says
//     unpartitioned, the table still hides every row, and the backfill silently
//     wrote nothing.
//
// relrowsecurity and relforcerowsecurity are catalog facts. rolsuper and
// rolbypassrls are catalog facts. Asking them costs one round trip and cannot
// disagree with what the statement will actually do.
func RLSBlockedTables(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error)
}, qualifiedNames []string) ([]string, error) {
	if len(qualifiedNames) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
SELECT n.nspname || '.' || c.relname
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname || '.' || c.relname = ANY($1)
   AND c.relrowsecurity
   AND c.relforcerowsecurity
   AND NOT (SELECT bool_or(r.rolsuper OR r.rolbypassrls)
              FROM pg_roles r WHERE r.rolname = current_user)`, qualifiedNames)
	if err != nil {
		return nil, fmt.Errorf("pg: probe row-level security: %w", err)
	}
	defer rows.Close()

	var blocked []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("pg: scan row-level security probe: %w", err)
		}
		blocked = append(blocked, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate row-level security probe: %w", err)
	}
	sort.Strings(blocked)
	return blocked, nil
}
