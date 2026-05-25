package entity

import (
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// buildGetSQL renders:
//
//	SELECT "col1", "col2", ... FROM "schema"."table" WHERE "pk" = $1 [AND "deleted_at" IS NULL]
func buildGetSQL(e *dsl.Entity) string {
	selectList := strings.Join(schema.QuoteAll(schema.FieldColumns(e)), ", ")
	table := schema.QualifiedTable(e)
	pkWhere := pkWhereClause(e, 1)
	softFilter := ""
	if e.SoftDeleteField != "" {
		softFilter = " AND " + schema.QuoteIdent(e.SoftDeleteField) + " IS NULL"
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s%s", selectList, table, pkWhere, softFilter)
}

// buildBatchGetSQL renders:
//
//	SELECT "col1", "col2", ... FROM "schema"."table" WHERE "pk" = ANY($1) [AND "deleted_at" IS NULL]
//
// Only used for single-PK entities; composite PKs fall back to
// individual GETs in the handler.
func buildBatchGetSQL(e *dsl.Entity) string {
	selectList := strings.Join(schema.QuoteAll(schema.FieldColumns(e)), ", ")
	table := schema.QualifiedTable(e)
	pkCols := schema.PKColumns(e)
	softFilter := ""
	if e.SoftDeleteField != "" {
		softFilter = " AND " + schema.QuoteIdent(e.SoftDeleteField) + " IS NULL"
	}
	if len(pkCols) > 0 {
		return fmt.Sprintf("SELECT %s FROM %s WHERE %s = ANY($1)%s",
			selectList, table, schema.QuoteIdent(pkCols[0].Name), softFilter)
	}
	// Defensive fallback — should not happen with a well-formed IR.
	return fmt.Sprintf("SELECT %s FROM %s WHERE \"id\" = ANY($1)%s",
		selectList, table, softFilter)
}

// buildQueryPrefix renders the SELECT ... FROM prefix; WHERE, ORDER BY,
// and LIMIT are appended at runtime by the query handler.
func buildQueryPrefix(e *dsl.Entity) string {
	selectList := strings.Join(schema.QuoteAll(schema.FieldColumns(e)), ", ")
	table := schema.QualifiedTable(e)
	return fmt.Sprintf("SELECT %s FROM %s", selectList, table)
}

// buildInsertSQL renders:
//
//	INSERT INTO "schema"."table" ("col1", "col2", ...) VALUES ($1, COALESCE($2::TYPE, default), ...) RETURNING "pk"
//
// Columns with declared defaults get COALESCE wrapping with type casts.
func buildInsertSQL(e *dsl.Entity) string {
	table := schema.QualifiedTable(e)
	insertCols := schema.InsertColumns(e)
	quotedCols := strings.Join(schema.QuoteAll(insertCols), ", ")

	// Build placeholders with COALESCE for defaults.
	placeholders := make([]string, len(insertCols))
	for i, name := range insertCols {
		f := e.FindField(name)
		ph := fmt.Sprintf("$%d", i+1)
		if f != nil && f.Default != nil {
			placeholders[i] = fmt.Sprintf("COALESCE(%s::%s, %s)",
				ph, schema.SQLType(f.Type), schema.DefaultExpr(*f.Default))
		} else {
			placeholders[i] = ph
		}
	}
	phStr := strings.Join(placeholders, ", ")

	// RETURNING clause: PK columns.
	pkCols := schema.PKColumns(e)
	pkNames := make([]string, len(pkCols))
	for i, pk := range pkCols {
		pkNames[i] = schema.QuoteIdent(pk.Name)
	}
	returning := strings.Join(pkNames, ", ")

	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s",
		table, quotedCols, phStr, returning)
}

// buildUpdateSQL renders:
//
//	UPDATE "schema"."table" SET "col1" = $1, "col2" = $2, ... WHERE "pk" = $N
//
// PK, identity, and serial columns are excluded from the SET list.
// PK placeholder numbers start after the SET columns.
func buildUpdateSQL(e *dsl.Entity) string {
	table := schema.QualifiedTable(e)

	// SET assignments: non-PK, non-identity, non-serial columns.
	var sets []string
	idx := 0
	for _, f := range e.Fields {
		if schema.IsPKColumn(e, f.Name) || f.Identity || f.Serial {
			continue
		}
		idx++
		sets = append(sets, fmt.Sprintf("%s = $%d", schema.QuoteIdent(f.Name), idx))
	}

	if len(sets) == 0 {
		// All columns are PK/identity/serial — nothing to update.
		return ""
	}

	setClause := strings.Join(sets, ", ")
	pkWhere := pkWhereClause(e, idx+1)

	return fmt.Sprintf("UPDATE %s SET %s WHERE %s", table, setClause, pkWhere)
}

// buildDeleteSQL renders either:
//
//	DELETE FROM "schema"."table" WHERE "pk" = $1                          (hard delete)
//	UPDATE "schema"."table" SET "deleted_at" = now() WHERE "pk" = $1 AND "deleted_at" IS NULL  (soft delete)
func buildDeleteSQL(e *dsl.Entity) string {
	table := schema.QualifiedTable(e)
	pkWhere := pkWhereClause(e, 1)

	if e.SoftDeleteField != "" {
		qSoft := schema.QuoteIdent(e.SoftDeleteField)
		return fmt.Sprintf("UPDATE %s SET %s = now() WHERE %s AND %s IS NULL",
			table, qSoft, pkWhere, qSoft)
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s", table, pkWhere)
}

// pkWhereClause builds the WHERE predicate for matching one row by PK,
// starting at placeholder $startIdx. Handles both single-column and
// composite PKs.
func pkWhereClause(e *dsl.Entity, startIdx int) string {
	pkCols := schema.PKColumns(e)
	if len(pkCols) == 0 {
		return fmt.Sprintf("\"id\" = $%d", startIdx)
	}
	if len(pkCols) == 1 {
		return fmt.Sprintf("%s = $%d", schema.QuoteIdent(pkCols[0].Name), startIdx)
	}
	// Composite PK: (col1, col2, ...) = ($N, $N+1, ...)
	cols := make([]string, len(pkCols))
	phs := make([]string, len(pkCols))
	for i, pk := range pkCols {
		cols[i] = schema.QuoteIdent(pk.Name)
		phs[i] = fmt.Sprintf("$%d", startIdx+i)
	}
	return "(" + strings.Join(cols, ", ") + ") = (" + strings.Join(phs, ", ") + ")"
}
