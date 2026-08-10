package sqlvalidate

import (
	"errors"
	"fmt"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// ValidateEntityExpressions runs the forbidden-call gate over the SQL an entity
// declaration carries outside a query or procedure body.
//
// # Why this exists separately from validateBlock
//
// The gate was wired into validateBlock, which sees `query{}` bodies and
// procedure raw steps, and that was mistaken for "all caller-authored SQL". It
// is not. A CHECK expression is author-written SQL that atlantis emits verbatim
// into DDL (internal/codegen/sql.go, `CONSTRAINT <name> CHECK (<expr>)`), and
// PostgreSQL does not require a CHECK expression to be IMMUTABLE. So a call
// planted there executes on every INSERT and UPDATE of the row.
//
// That is worse than the residual risk `partition by` already accepts. Dropping
// the policy or removing `partition by` is visible in a plan diff; a
// set_config buried in a CHECK reads as an ordinary constraint, survives
// review, and rebinds the tenant of a transaction that was correctly bound —
// which defeats every other defence at once, including the once-only guard,
// because set_config writes the parameter without going through the setter.
//
// Reproduced on PostgreSQL 17.8 as a NOSUPERUSER NOBYPASSRLS role: bound to
// tenant A, an INSERT firing such a CHECK left the discriminator reading
// tenant B, and every later statement in the transaction read tenant B's rows.
//
// Partial-index predicates are included even though PostgreSQL refuses a
// volatile function there ("functions in index predicate must be marked
// IMMUTABLE"). Relying on that would make this gate's coverage depend on an
// unrelated PostgreSQL rule that atlantis does not control.
func ValidateEntityExpressions(e *dsl.Entity) error {
	var errs []error

	for _, c := range e.Checks {
		name := c.Name
		if name == "" {
			name = "<unnamed>"
		}
		if err := checkExpression(c.Expr,
			fmt.Sprintf("%s: check %s", e.ID(), name)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range e.Fields {
		if f.Check == "" {
			continue
		}
		if err := checkExpression(f.Check,
			fmt.Sprintf("%s: field %q check", e.ID(), f.Name)); err != nil {
			errs = append(errs, err)
		}
	}
	for i, idx := range e.Indexes {
		// Only the free-text form can carry a call. The legacy shapes are a
		// column, an operator and a literal — there is nowhere to put one.
		if idx.Where == nil || idx.Where.Text == "" {
			continue
		}
		if err := checkExpression(idx.Where.Text,
			fmt.Sprintf("%s: index %d predicate", e.ID(), i)); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// checkExpression applies the forbidden-call gate to one expression, parsed in
// the syntactic context codegen actually emits it into.
//
// # Why the context matters, and why the first version of this leaked
//
// It parsed `SELECT (<expr>)` and skipped anything that failed to parse, on the
// reasoning that PostgreSQL would reject it too. That reasoning is false,
// because the two are different strings. codegen emits
// `CONSTRAINT <name> CHECK (<expr>)` inside CREATE TABLE, so an expression that
// closes the CHECK's parenthesis early is a parse error as a SELECT and
// perfectly good DDL in place:
//
//	check "true) , CONSTRAINT ck_evil CHECK (set_config('atlantis.tenant','victim',true) IS NOT NULL"
//
// emits two constraints, the second of which rebinds the tenant on every write.
// An adversarial review executed exactly that against PostgreSQL 17.8 as a
// NOSUPERUSER NOBYPASSRLS role and read another tenant's row through it, while
// this function returned nil.
//
// So: parse it as the constraint it becomes, reject what does not parse, and
// require that it yields exactly one constraint. The last of those is the one
// that catches the injection — the forbidden-call walk would have found the
// planted set_config too, but only once the payload was parsed in a shape that
// contains it. Structure first, contents second.
func checkExpression(expr, context string) error {
	if expr == "" {
		return nil
	}
	// The column exists so the statement is well-formed with no constraint at
	// all; it is never emitted anywhere.
	const prefix = `CREATE TABLE atlantis_expr_probe (probe_col integer, CONSTRAINT probe_ck CHECK (`
	tree, err := pg.Parse(prefix + normalizeNamedParams(expr) + `))`)
	if err != nil {
		return fmt.Errorf("%s: does not parse as a CHECK constraint: %w. "+
			"atlantis emits this text verbatim inside CREATE TABLE, so an "+
			"expression that only parses in some other shape is either a typo or "+
			"an attempt to escape the constraint it is declared in", context, err)
	}
	if len(tree.Stmts) != 1 {
		return fmt.Errorf("%s: yields %d statements, not one. The expression "+
			"escapes the constraint it is declared in", context, len(tree.Stmts))
	}
	if n := countTableConstraints(tree.Stmts[0].GetStmt()); n != 1 {
		return fmt.Errorf("%s: yields %d table constraints, not one. The "+
			"expression closes its own CHECK and opens something else, which "+
			"atlantis would emit verbatim into the table definition", context, n)
	}

	var errs []error
	errs = append(errs, checkForbiddenCalls(tree.Stmts[0].GetStmt(), context)...)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// countTableConstraints counts the table-level constraints a parsed CREATE
// TABLE declares. One is the constraint we wrapped; anything more came from the
// expression closing its own parenthesis.
func countTableConstraints(stmt *pg.Node) int {
	create := stmt.GetCreateStmt()
	if create == nil {
		return -1
	}
	n := 0
	for _, elt := range create.GetTableElts() {
		if elt.GetConstraint() != nil {
			n++
		}
		// A column definition can carry inline constraints too, which is where
		// an injected `probe_col int CHECK (...)` would land.
		if col := elt.GetColumnDef(); col != nil {
			n += len(col.GetConstraints())
		}
	}
	return n
}
