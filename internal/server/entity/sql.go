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

// buildWriteBackSQL renders the same projection as buildGetSQL with NO
// soft-delete filter.
//
// Create and Update read the row back inside their own transaction, and they
// read a row they just wrote, so its existence is not in question and the
// soft-delete filter has nothing to decide. Applying it here loses writes:
// sqlUpdate carries no such filter and the soft-delete column is not excluded
// from the update set, so updating an already-soft-deleted row, or
// soft-deleting through Update, makes the read-back return nothing and the
// error propagate before the commit. The write rolls back, the caller sees an
// error, and a client retrying on error loops forever.
func buildWriteBackSQL(e *dsl.Entity) string {
	selectList := strings.Join(schema.QuoteAll(schema.FieldColumns(e)), ", ")
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s",
		selectList, schema.QualifiedTable(e), pkWhereClause(e, 1))
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
func buildInsertSQL(e *dsl.Entity, extraReturning []string) string {
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
	// Cross-entity invalidation columns ride along on the RETURNING clause.
	// The row is being returned anyway, so this costs no round trip, and it is
	// the only way to learn the parent key without a second query.
	returning := strings.Join(append(pkNames, schema.QuoteAll(extraReturning)...), ", ")

	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING %s",
		table, quotedCols, phStr, returning)
}

// buildUpdateSQL renders:
//
//	UPDATE "schema"."table" SET "col1" = $1, "col2" = $2, ... WHERE "pk" = $N
//
// PK, identity, and serial columns are excluded from the SET list.
// PK placeholder numbers start after the SET columns.
func buildUpdateSQL(e *dsl.Entity, extraReturning []string) string {
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

	// Soft-deleted rows are not updatable, for the same reason they are not
	// readable: `soft_delete by` means the row is gone as far as the API is
	// concerned, and every generated read already filters it out.
	//
	// Without this, Update is an undelete: the soft-delete column is in the SET
	// list above, so a caller holding the primary key clears it and brings back
	// a row Delete removed. Where soft-delete means revoked or deactivated,
	// that is a privilege-restore primitive.
	//
	// handleUpdate maps zero rows affected to ErrNotFound, so a caller updating
	// a deleted row gets the answer Get gives it.
	softFilter := ""
	if e.SoftDeleteField != "" {
		softFilter = " AND " + schema.QuoteIdent(e.SoftDeleteField) + " IS NULL"
	}

	stmt := fmt.Sprintf("UPDATE %s SET %s WHERE %s%s", table, setClause, pkWhere, softFilter)
	if len(extraReturning) > 0 {
		stmt += " RETURNING " + strings.Join(schema.QuoteAll(extraReturning), ", ")
	}
	return stmt
}

// buildDeleteSQL renders either:
//
//	DELETE FROM "schema"."table" WHERE "pk" = $1                          (hard delete)
//	UPDATE "schema"."table" SET "deleted_at" = now() WHERE "pk" = $1 AND "deleted_at" IS NULL  (soft delete)
func buildDeleteSQL(e *dsl.Entity, extraReturning []string) string {
	table := schema.QualifiedTable(e)
	pkWhere := pkWhereClause(e, 1)

	var stmt string
	if e.SoftDeleteField != "" {
		qSoft := schema.QuoteIdent(e.SoftDeleteField)
		stmt = fmt.Sprintf("UPDATE %s SET %s = now() WHERE %s AND %s IS NULL",
			table, qSoft, pkWhere, qSoft)
	} else {
		stmt = fmt.Sprintf("DELETE FROM %s WHERE %s", table, pkWhere)
	}
	if len(extraReturning) > 0 {
		stmt += " RETURNING " + strings.Join(schema.QuoteAll(extraReturning), ", ")
	}
	return stmt
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

// buildSelectInboundSQL reads the cross-entity invalidation columns for one
// row, locking it.
//
// Used before an UPDATE, to capture the parent key the row had going in.
// Reparenting a child — moving a CartItem from cart 1 to cart 2 — has to
// invalidate both carts, and the UPDATE's RETURNING clause reports only the new
// value. FOR UPDATE because the read and the write must see the same row: an
// unlocked read could observe a value another transaction is in the middle of
// changing, and invalidate a parent that was never this row's.
//
// Empty when the entity has no inbound rules, and the caller skips the query
// entirely — most entities never pay for this.
func buildSelectInboundSQL(e *dsl.Entity, cols []string) string {
	if len(cols) == 0 {
		return ""
	}
	// If every inbound column is part of the primary key, the pre-read buys
	// nothing. PK columns are excluded from the UPDATE's SET list, so the value
	// cannot change — reparenting is impossible for such an entity — and the
	// handler already holds the value in pkValues. The real consumer.CartItem
	// is exactly this shape (primary by cart_id, variant_id), so without this
	// every cart-line update paid an extra round trip and an extra row lock to
	// re-learn a value it had.
	pk := map[string]bool{}
	for _, c := range schema.PKColumns(e) {
		pk[c.Name] = true
	}
	allPK := true
	for _, c := range cols {
		if !pk[c] {
			allPK = false
			break
		}
	}
	if allPK {
		return ""
	}
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s FOR UPDATE",
		strings.Join(schema.QuoteAll(cols), ", "),
		schema.QualifiedTable(e),
		pkWhereClause(e, 1))
}
