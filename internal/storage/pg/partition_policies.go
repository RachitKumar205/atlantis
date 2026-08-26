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
// A table passes with relrowsecurity and relforcerowsecurity set and a
// restrictive policy scoping the declared partition column on both halves. The
// returned strings name the tables that fail, one line each; an empty result
// means every partitioned table isolates.
//
// Reports rather than refuses, in the same shape as DetectRolePrivileges.
func VerifyPartitionPolicies(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (runtime.Rows, error)
}, tables []PartitionedTable) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}

	// The live catalogue, not a second declaration. A correct declaration sits
	// over a table with no policy when the migration was planned and never
	// applied, when the database was adopted with the clause already written,
	// or when the policy was dropped out of band; none of those is a diff.
	//
	// Binding a tenant succeeds whether or not a policy exists, so probing from
	// inside the server reports healthy either way.

	// One round trip, not one per entity: this runs on the boot path.
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		names = append(names, t.Schema+"."+t.Table)
	}

	// The policy's predicates, plural, and every one of them.
	//
	// Counting rows in pg_policy answers "is there a policy", which is not the
	// question. Three shapes pass a count and isolate nothing: USING (true); a
	// predicate on the wrong column, reachable because changing the `partition
	// by` field is another differ gap that leaves the old policy in place; and a
	// FOR INSERT policy with no SELECT policy at all.
	//
	// One aggregated string per table fails two ways. Joining the predicate to
	// the command with a `|` breaks on a predicate that contains one —
	// `(a || b)`, `body ~ 'x|y'`, `(flags | 4)` — so the split takes the first
	// field and reports every such table broken, refusing boot and every hot
	// reload on a healthy deployment. Accepting a table as soon as any policy
	// matches passes a correct policy sitting beside `USING (true)`, and that
	// pair returns every tenant's rows, permissive policies being OR'd.
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
		// separately. USING governs which rows are visible, WITH CHECK which
		// rows may be written; a correct USING beside `WITH CHECK (true)`
		// reports healthy to a check that reads only polqual, and lets a
		// request bound to one tenant INSERT a row stamped with another.
		//
		// Only restrictive policies are asked. They AND, so isolation holds
		// once one of them scopes the column, whatever else is attached.
		//
		// Permissive policies are the caller's authorization model. At least
		// one must exist: a table carrying only restrictive policies admits no
		// rows at all.
		readable, writable, grants := 0, 0, 0
		for _, pol := range st.policies {
			if pol.permissive {
				grants++
				continue
			}
			if governsRead(pol.cmd) && t.Column != "" && predicateScopesColumn(pol.qual, t.Column) {
				readable++
			}
			if governsWrite(pol.cmd) {
				// For ALL and UPDATE a null WITH CHECK reuses USING as the
				// check; DELETE has no check half, INSERT no using half.
				check := pol.withCheck
				if check == "" {
					check = pol.qual
				}
				if t.Column != "" && predicateScopesColumn(check, t.Column) {
					writable++
				}
			}
		}

		switch {
		case !present:
			// A missing table is a finding, not a skip.
			//
			// These entities come from the IR checkpoint, which only
			// ApplyMigration writes, so an entity in it has been applied and
			// its table exists. Absence means either the table was dropped
			// underneath the schema, or this code computed a name the database
			// does not use.
			//
			// Skipping instead turns every name-computation mistake into a
			// silent pass — building the name with TableName rather than
			// EntityPhysicalTable makes every entity using `table "..."` miss
			// its lookup and report healthy.
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and no table %s exists. Either it was "+
					"dropped, or atlantis is looking up the wrong name — and a "+
					"lookup that misses cannot tell you the policy is absent",
				t.EntityID, qualified))
		case len(st.policies) == 0:
			// The remedy line carries the message. diffPartition sees the
			// clause, so `tide plan` shows a pending migration; a message
			// saying otherwise sends an operator away from the one command
			// that fixes this.
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` but %s carries no row-level security "+
					"policy. Every read returns every tenant's rows. Run `tide plan` — "+
					"the change is diffed, so a pending migration will show it. If the "+
					"plan is empty the policy was dropped outside atlantis, or this "+
					"database was adopted with the clause already declared",
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
				"%s declares `partition by %s` and %s has no RESTRICTIVE policy "+
					"scoping that column on reads. Tenant isolation lives in the "+
					"restrictive slot, where it ANDs with every grant; without one "+
					"the permissive policies on this table are the only thing "+
					"deciding what is visible, and every caller reads every "+
					"tenant's rows. Run `tide plan`",
				t.EntityID, t.Column, qualified))
		case writable == 0:
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by %s` and %s has a RESTRICTIVE policy "+
					"scoping reads but none scoping writes. Reads are bounded and "+
					"writes are not: a request bound to one tenant can INSERT or "+
					"UPDATE a row stamped with another, which it then cannot see. "+
					"The boundary needs WITH CHECK as well as USING",
				t.EntityID, t.Column, qualified))
		case grants == 0:
			// The opposite of an isolation failure. Restrictive policies only
			// subtract, so a table carrying the boundary and no grant admits
			// nothing at all, and every read returns zero rows with no error.
			problems = append(problems, fmt.Sprintf(
				"%s declares `partition by` and %s has tenant isolation but no "+
					"PERMISSIVE policy. Restrictive policies only ever narrow, so "+
					"this table currently denies every row to every caller. atlantis "+
					"emits a default grant beside the boundary; if it was dropped to "+
					"make room for your own access control, at least one grant has "+
					"to remain",
				t.EntityID, qualified))
		}
	}
	sort.Strings(problems)
	return problems, nil
}

// PartitionPolicyError turns findings into the error that stops startup, or nil.
//
// Separated from the query, as the other boot decisions are, so a test can
// reach the refusal without a database.
func PartitionPolicyError(problems []string, require bool) error {
	if len(problems) == 0 || !require {
		return nil
	}
	// No "refusing to start" in the message. The hot-reload hook reaches this
	// with the server already running; the caller supplies the framing.

	// Noun and verb agree: "1 partitioned entity has", not "1 ... have".
	noun, verb := "entities", "have"
	if len(problems) == 1 {
		noun, verb = "entity", "has"
	}
	return fmt.Errorf("%d partitioned %s %s no enforced row-level "+
		"security, and ATL_REQUIRE_TENANT_ISOLATION is set:\n  %s",
		len(problems), noun, verb, strings.Join(problems, "\n  "))
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
// A substring test over the emitted form,
// `<col> = atlantis.current_partition()[::cast]`. It catches the coarse failure
// — a predicate naming neither the column nor the function — and nothing finer.
//
// One predicate, not an aggregate of several: the caller holds the list and
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
// A plain substring test accepts a policy on the wrong column whenever the
// declared name appears anywhere in the predicate text.
// `(other = atlantis.current_partition())` then scopes the columns `t`, `at`
// and `partition`, the last because the name sits inside
// `atlantis.current_partition()` itself, and `(tenant_backup = ...)` scopes
// `tenant`.
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
// Answered from the catalogue, not the declaration: it is a property of the
// table and the role. Reading `partition by` instead is wrong three ways —
// the clause declared with no policy on the table, a BYPASSRLS or superuser
// role that the policy does not apply to, and a dropped clause whose policy
// survives because removal emits nothing.
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
