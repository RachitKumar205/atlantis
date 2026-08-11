// Package sqlvalidate is the pg_query_go-backed validator for the raw
// SQL embedded in `query` and `procedure` declarations. It runs at
// tidectl plan time (or any caller-side build that wants deep validation),
// not on every codegen pass — pg_query_go is a CGO dep and we keep it
// off the hot codegen path. The dep-free pre-checks in
// internal/dsl/ir.go cover the most common failures ($arg references,
// touches() resolution, typed-step column existence); this package
// adds the deeper, PG-aware checks.
//
// Checks performed:
//
//   - SQL parses with the same grammar Postgres uses (pg_query is a
//     fork of the PG parser).
//   - Every table referenced in a FROM / JOIN / UPDATE / DELETE /
//     INSERT clause resolves to an entity in the merged IR. Caller
//     queries cannot reference tables that do not exist in the
//     schema — typos and stale references surface at plan time
//     rather than at first execution.
//   - Statement shape matches the declaration form: queries must use
//     a SELECT statement (or a CTE that resolves to one); procedure
//     raw-SQL steps may use SELECT, INSERT, UPDATE, or DELETE, but
//     not DDL (CREATE, ALTER, DROP, GRANT, etc.) — DDL must flow
//     through the autogen migration path so atlantis retains
//     authority over the schema.
//
// Future checks (deferred until a callsite demands them):
//
//   - Column-existence validation. Walking pg_query's expression tree
//     to extract every ColumnRef and binding it back to entity fields
//     means handling aliases, sub-selects, lateral joins, and column
//     ambiguity — non-trivial. The IR-level `touches()` check + the
//     test pass at tidectl plan time catches most regressions in
//     practice. When a callsite needs tighter column validation, we
//     add it here.
//   - Cost estimation via EXPLAIN. Requires a live PG connection and
//     belongs in tidectl plan rather than here.
package sqlvalidate

import (
	"errors"
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Mode toggles between the validation flavors for the two raw-SQL
// contexts. Queries permit only SELECT shapes; procedure raw steps
// additionally permit DML.
type Mode int

const (
	// ModeQuery is the validation for `query { sql touches(...) { ... } }`
	// blocks. Statement must be SELECT (or CTE/WITH wrapping a SELECT).
	ModeQuery Mode = iota
	// ModeProcedureStep is the validation for `procedure { steps { sql
	// touches(...) { ... } } }` blocks. DML allowed; DDL still rejected.
	ModeProcedureStep
)

// ValidateCustomQuery runs the deep validator over a single CustomQuery.
// Errors are aggregated rather than fast-failing so plan-time output
// surfaces every problem in one pass.
func ValidateCustomQuery(ir *dsl.IR, q *dsl.CustomQuery) error {
	var errs []error
	tableSet := buildTableSet(ir)
	if e := validateBlock(q.SQL, ModeQuery, tableSet, fmt.Sprintf("query %s", q.ID()), q.Touches); e != nil {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// ValidateCustomProcedure runs the deep validator over every raw step
// inside a procedure. Typed steps are validated structurally at IR
// lowering — they don't reach pg_query_go because their SQL is
// generated from existing entity templates.
func ValidateCustomProcedure(ir *dsl.IR, p *dsl.CustomProcedure) error {
	var errs []error
	tableSet := buildTableSet(ir)
	for i, step := range p.Steps {
		if step.Raw == nil {
			continue
		}
		ctx := fmt.Sprintf("procedure %s step %d", p.ID(), i+1)
		if e := validateBlock(step.Raw.SQL, ModeProcedureStep, tableSet, ctx, step.Raw.Touches); e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// validateBlock runs the parser pass + statement-kind + table-reference
// checks against one raw SQL body.
//
// The body uses DSL-style named placeholders (`$consumer_id`), but
// pg_query_go's parser requires PG positional shape (`$1`, `$2`, ...).
// We normalize before parsing so pg_query sees syntactically valid
// PG. The IR layer already validated that every $name resolves to a
// declared input, so this rewrite is purely syntactic — it does not
// change semantics or table/column references.
func validateBlock(sql string, mode Mode, tables map[string]string, context string, touches []string) error {
	tree, err := pg.Parse(normalizeNamedParams(sql))
	if err != nil {
		return fmt.Errorf("%s: SQL parse failed: %w", context, err)
	}
	if len(tree.Stmts) == 0 {
		return fmt.Errorf("%s: SQL body is empty", context)
	}

	var errs []error
	var refTables []string
	for i, rawStmt := range tree.Stmts {
		stmt := rawStmt.GetStmt()
		if err := checkStatementKind(stmt, mode); err != nil {
			errs = append(errs, fmt.Errorf("%s: statement %d: %w", context, i+1, err))
			continue
		}
		// Before anything that only reports: a forbidden call is a security
		// refusal, not a lint. See forbidden_calls.go.
		errs = append(errs, checkForbiddenCalls(stmt, fmt.Sprintf("%s: statement %d", context, i+1))...)
		// Scope-aware: see realTableRefs. A flat CTE-name set let a dummy CTE
		// in an unrelated subquery hide a top-level read, and let a DML target
		// masquerade as a CTE while writing the real table.
		for _, ref := range realTableRefs(stmt) {
			canonical, ok := resolveTable(tables, ref)
			if !ok {
				errs = append(errs, fmt.Errorf("%s: SQL references unknown table %q (no entity maps to this name)", context, ref))
				continue
			}
			refTables = append(refTables, canonical)
		}
	}

	// touches() must be a superset of every entity actually referenced
	// in the SQL. The cache layer derives generation bumps from
	// touches; if the SQL reads or writes an entity that isn't in
	// touches, that entity's bumps won't fire and cached results go
	// stale silently.
	if err := checkTouchesCoverage(touches, refTables, context); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// checkStatementKind enforces the allowed top-level statement shapes
// per validation mode. Anything not on the allow-list is rejected with
// a pointed error — including DDL, which atlantis routes through
// the autogen migration path, not the runtime query surface.
func checkStatementKind(stmt *pg.Node, mode Mode) error {
	switch n := stmt.GetNode().(type) {
	case *pg.Node_SelectStmt:
		return nil
	case *pg.Node_InsertStmt, *pg.Node_UpdateStmt, *pg.Node_DeleteStmt:
		if mode == ModeQuery {
			return fmt.Errorf("query{} bodies must be SELECT statements; got %s", statementName(n))
		}
		return nil
	default:
		return fmt.Errorf("disallowed statement kind %T (DDL and other non-DML shapes go through tidectl migrations, not raw SQL)", n)
	}
}

// statementName returns a short human-readable name for a statement
// node. Used in error messages.
func statementName(n any) string {
	switch n.(type) {
	case *pg.Node_SelectStmt:
		return "SELECT"
	case *pg.Node_InsertStmt:
		return "INSERT"
	case *pg.Node_UpdateStmt:
		return "UPDATE"
	case *pg.Node_DeleteStmt:
		return "DELETE"
	}
	return fmt.Sprintf("%T", n)
}

// collectTableRefs returns every table name a parsed statement references.
//
// # Why this walks by reflection
//
// It used to walk by hand, descending only into the node types it named:
// FromClause, TargetList, WhereClause, HavingClause, CTEs, set-op arms, and a
// handful of others. Anything composite stopped it. There was no case for
// BoolExpr, A_Expr, FuncCall, CaseExpr, SortClause, GroupClause, LimitCount or
// ValuesLists, so a table referenced through any of them was invisible.
//
// Measured against 16 ordinary SQL shapes on pg_query_go v6.2.2: the
// hand-written walk found the table in 2 of them and missed 14. Only a plain
// FROM and a bare IN-subquery survived. Among the misses:
//
//	WHERE active AND id IN (SELECT ... FROM orders)
//	WHERE id = (SELECT max(...) FROM orders)
//	SELECT coalesce((SELECT ... FROM orders), 0)
//	ORDER BY (SELECT ... FROM orders)
//	UPDATE ... SET x = (SELECT ... FROM orders)
//
// That is not only a stale cache. checkTouchesCoverage uses this list to decide
// whether the author's `touches()` is complete, and internal/server/entity
// derives from `touches()` whether a custom query needs the caller's tenant
// bound. A missed table meant a query joining a partitioned entity ran with no
// tenant bound at all — returning nothing on a role row-level security applies
// to, and every tenant's rows on a role that bypasses it.
//
// So this walks the protobuf message graph and collects every RangeVar it
// finds, wherever it sits. A RangeVar is precisely a table reference; subquery
// aliases are RangeSubselect and do not appear. CTE references do appear and
// are filtered by the caller against collectCTENames — which had to become
// reflective too, or the two walks disagree and legal SQL is rejected.
//
// Same remedy, and same reason, as the walk in forbidden_calls.go: a check
// derived from the structure cannot fall behind the structure.
func collectTableRefs(stmt *pg.Node) []string {
	// Two node types hold a RangeVar that is not a table reference, and both
	// have to be skipped or ordinary SQL stops applying:
	//
	//   FOR UPDATE OF o      LockingClause.lockedRels holds the ALIAS `o`
	//   SELECT ... INTO t    IntoClause.rel names a table being CREATED
	//
	// The first is the canonical read-modify-write procedure step. Collecting
	// it reported `unknown table "o"`, so a schema that applied yesterday
	// failed today and the obvious fix was to delete the row lock. Found by
	// review, after the exhaustive walk replaced a hand-written one that had
	// never descended far enough to see either.
	skip := nonTableRangeVars(stmt)

	var out []string
	walkMessages(stmt.ProtoReflect(), func(m protoreflect.Message) {
		rv, ok := m.Interface().(*pg.RangeVar)
		if !ok || rv == nil || skip[m] {
			return
		}
		out = append(out, normalizeTableRef(rv))
	})
	return out
}

// collectCTENames walks a parsed statement and returns the set of
// names defined by WITH clauses anywhere in the tree (including
// nested SELECTs and DML statements). Names are lower-cased because
// PG identifiers are case-insensitive by default. The resulting set
// is consulted before reporting an "unknown table" error so a CTE
// alias used downstream in the same statement doesn't surface as a
// schema gap.
// collectCTENames returns every name bound by a WITH clause anywhere in a
// statement, so the caller can tell a CTE reference from a missing table.
//
// Reflective, and it has to be: collectTableRefs became reflective and this did
// not, so the two stopped agreeing. A CTE referenced from behind a BoolExpr —
//
//	SELECT id FROM products WHERE active AND id IN
//	  (WITH recent AS (SELECT product_id FROM orders) SELECT product_id FROM recent)
//
// — had its REFERENCE collected as a table while its NAME was not collected as
// a CTE, so validation rejected `unknown table "recent"` on SQL that applied
// before the change. Two walks over the same tree have to descend equally far
// or the difference between them becomes a false rejection.
// nonTableRangeVars marks the RangeVars that do not denote a table reference.
func nonTableRangeVars(stmt *pg.Node) map[protoreflect.Message]bool {
	skip := map[protoreflect.Message]bool{}
	walkMessages(stmt.ProtoReflect(), func(m protoreflect.Message) {
		switch n := m.Interface().(type) {
		case *pg.LockingClause:
			for _, r := range n.GetLockedRels() {
				if rv := r.GetRangeVar(); rv != nil {
					skip[rv.ProtoReflect()] = true
				}
			}
		case *pg.IntoClause:
			if rv := n.GetRel(); rv != nil {
				skip[rv.ProtoReflect()] = true
			}
		}
	})
	return skip
}

// realTableRefs returns every reference in stmt that denotes an actual table,
// resolving CTE names the way PostgreSQL does rather than by flat name.
//
// # Why a flat name set is not enough
//
// collectCTENames gathers every CTE name anywhere in the statement into one
// set, and a reference matching any of them was skipped. PostgreSQL scopes a
// CTE name to the WITH that declares it. Two shapes got through, both executed
// against a live database as the atlantis role:
//
//	SELECT ir FROM ir_checkpoint WHERE id = 1
//	  AND EXISTS (WITH ir_checkpoint AS (SELECT 1) SELECT 1 FROM ir_checkpoint);
//
// The dummy CTE is declared inside an unrelated subquery and cannot shadow the
// top-level reference, but the flat set did not know that — so the read of the
// real IR checkpoint was skipped. And:
//
//	WITH ir_checkpoint AS (SELECT 1) UPDATE ir_checkpoint SET ir = $payload;
//
// PostgreSQL forbids a CTE as an UPDATE/DELETE/INSERT target, so the name
// resolves to the REAL relation while the validator saw a CTE.
//
// A skipped reference never reaches refTables, so checkTouchesCoverage stops
// requiring the entity in `touches()`, and internal/server/entity derives from
// `touches()` whether a custom query binds the caller's tenant. The shadowed
// statement then runs UNBOUND, on the bare pool. Both shapes passed
// ValidateCustomQuery and ValidateCustomProcedure.
//
// So scope is carried down the tree: a WITH extends it for that statement's
// subtree only, and a DML target never consults it at all.
func realTableRefs(stmt *pg.Node) []string {
	skip := nonTableRangeVars(stmt)
	targets := dmlTargetRangeVars(stmt)

	var out []string
	var walk func(m protoreflect.Message, scope map[string]bool)
	walk = func(m protoreflect.Message, scope map[string]bool) {
		if !m.IsValid() {
			return
		}
		if names := withClauseNames(m); len(names) > 0 {
			next := make(map[string]bool, len(scope)+len(names))
			for k := range scope {
				next[k] = true
			}
			for _, n := range names {
				next[n] = true
			}
			scope = next
		}
		if rv, ok := m.Interface().(*pg.RangeVar); ok && rv != nil {
			if skip[m] {
				return
			}
			name := normalizeTableRef(rv)
			// A qualified name always means the table: PostgreSQL resolves a
			// CTE by bare identifier only. A quoted CTE name may itself contain
			// a dot, which is how `WITH "atlantis.jobs" AS (...)` once put a
			// CTE into a qualified table's slot.
			if !strings.Contains(name, ".") && scope[strings.ToLower(name)] && !targets[m] {
				return
			}
			out = append(out, name)
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.IsMap() || fd.Kind() != protoreflect.MessageKind {
				return true
			}
			if fd.IsList() {
				l := v.List()
				for i := 0; i < l.Len(); i++ {
					walk(l.Get(i).Message(), scope)
				}
				return true
			}
			walk(v.Message(), scope)
			return true
		})
	}
	walk(stmt.ProtoReflect(), map[string]bool{})
	return out
}

// withClauseNames returns the CTE names a message declares, for any node type
// carrying a with_clause — SELECT, INSERT, UPDATE, DELETE and MERGE all do.
//
// Read by field descriptor rather than by a type switch, so a node type that
// gains a WITH clause does not silently stop extending scope.
func withClauseNames(m protoreflect.Message) []string {
	fd := m.Descriptor().Fields().ByName("with_clause")
	if fd == nil || fd.IsList() || fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
		return nil
	}
	wc, ok := m.Get(fd).Message().Interface().(*pg.WithClause)
	if !ok || wc == nil {
		return nil
	}
	var names []string
	for _, c := range wc.GetCtes() {
		if cte := c.GetCommonTableExpr(); cte != nil && cte.GetCtename() != "" {
			names = append(names, strings.ToLower(cte.GetCtename()))
		}
	}
	return names
}

// dmlTargetRangeVars marks the relation each INSERT, UPDATE, DELETE and MERGE
// writes to.
//
// PostgreSQL will not resolve one of these to a CTE — `WITH t AS (...) UPDATE
// t` writes the real table — so these references must never be skipped as CTE
// names however the WITH clause is spelled.
func dmlTargetRangeVars(stmt *pg.Node) map[protoreflect.Message]bool {
	out := map[protoreflect.Message]bool{}
	walkMessages(stmt.ProtoReflect(), func(m protoreflect.Message) {
		var rv *pg.RangeVar
		switch n := m.Interface().(type) {
		case *pg.UpdateStmt:
			rv = n.GetRelation()
		case *pg.DeleteStmt:
			rv = n.GetRelation()
		case *pg.InsertStmt:
			rv = n.GetRelation()
		case *pg.MergeStmt:
			rv = n.GetRelation()
		}
		if rv != nil {
			out[rv.ProtoReflect()] = true
		}
	})
	return out
}

func collectCTENames(stmt *pg.Node) map[string]struct{} {
	out := map[string]struct{}{}
	walkMessages(stmt.ProtoReflect(), func(m protoreflect.Message) {
		cte, ok := m.Interface().(*pg.CommonTableExpr)
		if !ok || cte == nil || cte.GetCtename() == "" {
			return
		}
		out[strings.ToLower(cte.GetCtename())] = struct{}{}
	})
	return out
}

// normalizeTableRef produces a canonical "schema.table" string for a
// RangeVar reference. Unschema-qualified references default to the
// SQL's notional namespace; the resolver layer maps both forms to
// entity ids.
func normalizeTableRef(rv *pg.RangeVar) string {
	if rv.Schemaname != "" {
		return rv.Schemaname + "." + rv.Relname
	}
	return rv.Relname
}

// resolveTable maps a SQL table reference back to an entity id. The
// mapping mirrors the table-name convention in internal/codegen/sql.go:
// `tableName(e) = e.Namespace + "_" + snake_case(e.Name)`, inside the
// `atlantis` schema. References can therefore appear as any of:
//
//   - `atlantis.consumer_account` — fully qualified
//   - `consumer_account` — unqualified, schema defaults to search_path
//   - other schemas — rejected (only atlantis is owned by the codegen)
//
// resolveTable looks up a SQL table reference (with or without schema
// prefix) against the precomputed table set. Accepted forms:
//
//   - Fully qualified `schema.table` — looked up verbatim. Honors both
//     the default `atlantis.<namespace>_<snake>` form (for entities
//     with no `table "..."` override) and any override the entity set.
//   - Bare `table` (no schema prefix) — looked up against the entry
//     keyed by the bare table component. This keeps short references
//     working when an entity is in `public` via `table "name"`.
func resolveTable(tables map[string]string, ref string) (string, bool) {
	ref = strings.ToLower(ref)
	id, ok := tables[ref]
	return id, ok
}

// buildTableSet computes the SQL-table-name -> entity-id lookup once
// per validation pass. Lower-cases the keys so PG's case-insensitive
// identifier matching works transparently.
//
// Each entity is registered under every form a user can legitimately
// type in their custom-query SQL:
//
//   - `schema.table` (fully qualified) — always populated, using the
//     entity's actual on-disk location: either the `table "..."`
//     override or the default `atlantis.<namespace>_<snake>`.
//   - `table` (bare) — populated when there's no override (matches the
//     bare snake-case form) AND when there IS an override (matches the
//     table portion of the override). Lets short references work in
//     either world.
func buildTableSet(ir *dsl.IR) map[string]string {
	out := make(map[string]string, len(ir.Entities)*2)
	for i := range ir.Entities {
		e := &ir.Entities[i]
		entityID := e.ID()
		var qualified, bare string
		if e.TableName != "" {
			if dot := strings.IndexByte(e.TableName, '.'); dot >= 0 {
				qualified = strings.ToLower(e.TableName)
				bare = strings.ToLower(e.TableName[dot+1:])
			} else {
				qualified = strings.ToLower("public." + e.TableName)
				bare = strings.ToLower(e.TableName)
			}
		} else {
			bare = strings.ToLower(e.Namespace + "_" + snakeCase(e.Name))
			qualified = "atlantis." + bare
		}
		out[qualified] = entityID
		out[bare] = entityID
	}
	return out
}

// checkTouchesCoverage compares the entities the SQL actually
// references against the touches() declaration. If the SQL touches an
// entity the engineer didn't list, that entity's cache won't be
// bumped on write — the validator catches this before the misconfig
// reaches production.
//
// The reverse (touches lists an entity the SQL never references) is
// caught at IR lowering time as "input never referenced in SQL"-style
// guards on the touches list; this function only fires on
// undeclared references.
func checkTouchesCoverage(declared []string, referenced []string, context string) error {
	if len(referenced) == 0 {
		return nil
	}
	declSet := make(map[string]bool, len(declared))
	for _, d := range declared {
		declSet[d] = true
	}
	seen := map[string]bool{}
	var missing []string
	for _, r := range referenced {
		if seen[r] {
			continue
		}
		seen[r] = true
		if !declSet[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s: SQL references %s but touches() does not declare them; cache invalidation would be silently stale", context, strings.Join(missing, ", "))
	}
	return nil
}

// normalizeNamedParams replaces DSL-style `$ident` placeholders with PG
// positional `$1` so pg_query_go's parser accepts the body. The
// validator only cares about table / column / statement-kind
// resolution; the positional index doesn't matter for that. The
// rewrite is intentionally crude — it does not track which name maps
// to which positional index, because the IR layer already validated
// argument names against the declared input list, and the codegen
// layer does the real index assignment at SQL emit time.
//
// Skipped contexts:
//
//   - Inside single-quoted SQL strings (`'...'`). `'$foo'` is a literal
//     value, not a parameter. Same for embedded `”` escapes.
//
//   - Inside double-quoted SQL identifiers (`"...$foo..."`). Real PG
//     identifiers can contain `$` so we leave them untouched.
//
//   - Already-numeric `$<digit>` shapes pass through unchanged.
//
//   - Inside dollar-quoted strings (`$tag$ ... $tag$`, `$$ ... $$`).
//
//   - Inside `--` line comments and `/* */` block comments, which PostgreSQL
//     nests.
//
// The last two were the note "if a real callsite hits this we'll add
// handling", and a callsite hit it. `check` and `index by expr` are audited
// against the ALREADY-STORED IR at boot, and the audit is fatal under
// ATL_REQUIRE_TENANT_ISOLATION. So a declaration PostgreSQL has been enforcing
// for months stopped the server starting:
//
//	check "position($tag$@$tag$ in email) > 0"   -- $tag rewritten to $1
//	check "x > 0 -- don''t allow zero"           -- apostrophe opened a string
//
// The first rewrote `$tag` to `$1` and destroyed the dollar-quote; the second
// let an apostrophe inside a comment swallow the wrapper's closing parens. Both
// produced a syntax error, and the gate treats a syntax error as a refusal.
func normalizeNamedParams(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch c {
		case '\'':
			// Skip a single-quoted string verbatim, handling `''` as
			// the in-string escape for a literal quote.
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					b.WriteByte('\'')
					if i+1 < len(sql) && sql[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '"':
			// Skip a double-quoted identifier verbatim, with `""` as
			// the in-string escape.
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '"' {
					b.WriteByte('"')
					if i+1 < len(sql) && sql[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '-':
			// `--` runs to end of line. An apostrophe inside one would
			// otherwise open a string that never closes.
			if i+1 < len(sql) && sql[i+1] == '-' {
				for i < len(sql) && sql[i] != '\n' {
					b.WriteByte(sql[i])
					i++
				}
				i--
				continue
			}
			b.WriteByte(c)
		case '/':
			// `/* */`, which PostgreSQL nests: an inner `*/` closes only the
			// inner comment.
			if i+1 < len(sql) && sql[i+1] == '*' {
				depth := 1
				b.WriteString("/*")
				i += 2
				for i < len(sql) && depth > 0 {
					if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
						depth++
						b.WriteString("/*")
						i += 2
						continue
					}
					if i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/' {
						depth--
						b.WriteString("*/")
						i += 2
						continue
					}
					b.WriteByte(sql[i])
					i++
				}
				i--
				continue
			}
			b.WriteByte(c)
		case '$':
			// A dollar-quoted string, not a parameter. `$tag$` and `$$` open
			// one; the tag may not begin with a digit, which is what keeps
			// `$1` positional. Copied verbatim to the matching close.
			if tag, ok := dollarQuoteTag(sql, i); ok {
				if end := strings.Index(sql[i+len(tag):], tag); end >= 0 {
					stop := i + len(tag) + end + len(tag)
					b.WriteString(sql[i:stop])
					i = stop - 1
					continue
				}
				// Unterminated. Leave the rest alone rather than rewriting
				// inside it; the parser will report the real error.
				b.WriteString(sql[i:])
				return b.String()
			}
			// `$<digit>...` is PG positional; pass through.
			if i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				b.WriteByte('$')
				continue
			}
			// `$<ident>` is the DSL-named form; rewrite to `$1`.
			if i+1 < len(sql) && (isLetterOrUnderscore(sql[i+1])) {
				// Consume the ident.
				j := i + 1
				for j < len(sql) && isIdentChar(sql[j]) {
					j++
				}
				b.WriteString("$1")
				i = j - 1
				continue
			}
			b.WriteByte('$')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isLetterOrUnderscore(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isIdentChar(c byte) bool {
	return isLetterOrUnderscore(c) || (c >= '0' && c <= '9')
}

// snakeCase mirrors the entity name → SQL table conversion used by the
// codegen layer. Kept in lockstep with internal/codegen/sql.go's
// snakeCase — the entity SavedOutfit lowers to `saved_outfit`, so a
// query referencing `consumer_saved_outfit` resolves to consumer.SavedOutfit.
func snakeCase(s string) string {
	var out []rune
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			out = append(out, '_')
		}
		if r >= 'A' && r <= 'Z' {
			r = r + ('a' - 'A')
		}
		out = append(out, r)
	}
	return string(out)
}

// dollarQuoteTag reports the opening delimiter of a dollar-quoted string at i,
// e.g. `$$` or `$tag$`.
//
// PostgreSQL forbids a tag starting with a digit, which is exactly what keeps
// `$1` a positional parameter rather than the start of a quote.
func dollarQuoteTag(sql string, i int) (string, bool) {
	if i >= len(sql) || sql[i] != '$' {
		return "", false
	}
	j := i + 1
	if j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
		return "", false
	}
	for j < len(sql) && (isLetterOrUnderscore(sql[j]) ||
		(sql[j] >= '0' && sql[j] <= '9')) {
		j++
	}
	if j < len(sql) && sql[j] == '$' {
		return sql[i : j+1], true
	}
	return "", false
}
