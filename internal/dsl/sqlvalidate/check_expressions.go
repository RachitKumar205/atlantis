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
// Separate from validateBlock, which sees only `query{}` bodies and procedure
// raw steps. internal/codegen/sql.go emits a CHECK expression verbatim into DDL
// as `CONSTRAINT <name> CHECK (<expr>)`, and PostgreSQL does not require it to
// be IMMUTABLE, so a call planted there runs on every INSERT and UPDATE.
//
// set_config writes the parameter without going through set_partition, so it
// rebinds the tenant of a transaction that was correctly bound and the
// once-only guard never sees it. On PostgreSQL 17.8, as a NOSUPERUSER
// NOBYPASSRLS role bound to tenant A, an INSERT firing such a CHECK left the
// discriminator reading tenant B for the rest of the transaction.
//
// Partial-index predicates are covered too. PostgreSQL refuses a volatile
// function there — "functions in index predicate must be marked IMMUTABLE" —
// but that is its rule to change, not atlantis's.
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
		if f.Check != "" {
			if err := checkExpression(f.Check,
				fmt.Sprintf("%s: field %q check", e.ID(), f.Name)); err != nil {
				errs = append(errs, err)
			}
		}
		// `default raw "<sql>"` is one caller-authored SQL surface among
		// several: query bodies, procedure steps, table CHECKs, field CHECKs,
		// partial-index predicates, backfill expressions and index expressions.
		//
		// That list is not complete. Author text also reaches the migration's
		// `--` comment lines, where a newline ends the comment and leaves the
		// rest at top level; sqlBuilder.commentf closes that one.
		//
		// Coverage is uneven as well. `backfill` is gated from
		// internal/server/admin/backfill.go, not at plan time and not in the
		// boot audit: AuditForbiddenCalls runs ValidateEntityExpressions, which
		// never reads Field.Backfill.
		//
		// This surface runs on every insert that omits the column, not only at
		// CREATE TABLE — internal/server/entity inlines it into the generated
		// INSERT as COALESCE($n::type, <raw expr>).
		if f.Default != nil && f.Default.Kind == dsl.DefaultIRRaw && f.Default.Str != "" {
			if err := checkExpression(f.Default.Str,
				fmt.Sprintf("%s: field %q default", e.ID(), f.Name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for i, idx := range e.Indexes {
		// `index by expr "<sql>"` is emitted as `((<expr>))` inside CREATE
		// INDEX, and internal/dsl/ir.go leaves the expression to Postgres at
		// migration time.
		//
		// Postgres validating it later is not a gate: the escape is a statement
		// boundary, not a bad expression.
		//
		//	index by expr "lower(email)); DROP POLICY IF EXISTS <policy> ON <table>; CREATE INDEX zz ON <table> ((1"
		//
		// applies cleanly and drops the tenant-isolation policy off the table.
		for j, f := range idx.Fields {
			if !f.IsExpr || f.Expr == "" {
				continue
			}
			if err := checkIndexExpression(f.Expr,
				fmt.Sprintf("%s: index %d expression %d", e.ID(), i, j)); err != nil {
				errs = append(errs, err)
			}
		}

		// Only the free-text form of a predicate can carry a call. The legacy
		// shapes are a column, an operator and a literal — nowhere to put one.
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
// Parsing `SELECT (<expr>)` and skipping what fails is unsound: codegen emits
// `CONSTRAINT <name> CHECK (<expr>)` inside CREATE TABLE, so an expression that
// closes the CHECK's parenthesis early is a parse error as a SELECT and valid
// DDL in place.
//
//	check "true) , CONSTRAINT ck_evil CHECK (set_config('atlantis.tenant','victim',true) IS NOT NULL"
//
// emits two constraints, the second of which rebinds the tenant on every write.
// On PostgreSQL 17.8, as a NOSUPERUSER NOBYPASSRLS role, a read through it
// returned another tenant's row while this function returned nil.
//
// So the checks are structural first: parse it as the constraint it becomes,
// reject what does not parse, and require exactly one constraint. The
// forbidden-call walk finds the planted set_config only once the payload is
// parsed in a shape that contains it.
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
// TABLE declares. One is the probe's own wrapper; anything more came from the
// expression closing that parenthesis.
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

// checkIndexExpression applies the gate to an index expression, parsed in the
// shape codegen emits it into.
//
// A separate function from checkExpression because the emission context is
// different — `CREATE INDEX ... ON t ((<expr>))`, not a CHECK constraint — and
// the whole lesson of the CHECK case is that a parse which succeeds in some
// other shape proves nothing about the place the text lands.
//
// One statement, one index. The escape this exists to stop closes the emitted
// parenthesis and appends further DDL, which shows up as extra statements.
func checkIndexExpression(expr, context string) error {
	if expr == "" {
		return nil
	}
	const prefix = `CREATE INDEX atlantis_expr_probe ON atlantis_probe_tbl ((`
	tree, err := pg.Parse(prefix + normalizeNamedParams(expr) + `))`)
	if err != nil {
		return fmt.Errorf("%s: does not parse as an index expression: %w. "+
			"atlantis emits this text verbatim inside CREATE INDEX, so an "+
			"expression that only parses in some other shape is either a typo or "+
			"an attempt to escape the statement it is declared in", context, err)
	}
	if len(tree.Stmts) != 1 {
		return fmt.Errorf("%s: yields %d statements, not one. The expression "+
			"closes its own CREATE INDEX and appends further DDL, which atlantis "+
			"would apply — including DDL that drops the tenant-isolation policy "+
			"every other check in this package protects", context, len(tree.Stmts))
	}
	if tree.Stmts[0].GetStmt().GetIndexStmt() == nil {
		return fmt.Errorf("%s: does not yield a CREATE INDEX. The expression "+
			"escapes the statement it is declared in", context)
	}
	if errs := checkForbiddenCalls(tree.Stmts[0].GetStmt(), context); len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
