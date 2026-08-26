// Package backfill is the Phase-2 runtime for `tide apply --backfill`. It
// drains atlantis.backfill_field_state rows by running a chunked UPDATE
// template, the splicer, against the caller's table.
//
// Once every field in a plan is complete, the worker runs Phase 3: SET NOT NULL
// on the deferred fields, having first compared the stored ir_checkpoint hash
// against the live one. Concurrent workers are separated by FOR UPDATE SKIP
// LOCKED on the row and a compare-and-swap on backfill_plan.status, not by an
// advisory lock.
package backfill

import (
	"fmt"
	"strings"
)

// ChunkSQL builds the parameterized chunked-UPDATE statement for one
// backfill field. Parameters at runtime:
//
//	$1 — last_pk (bigint; 0 for the initial chunk)
//	$2 — chunk size (int)
//
// The expression is embedded verbatim, and admin.BeginBackfillPlan is the only
// gate on it: sqlvalidate.ValidateBackfillExpression runs there, before the row
// reaches backfill_field_state, rejecting subqueries, non-whitelisted function
// calls and unknown column refs. Nothing revalidates on the way back out, so a
// row written by any other path is executed as written.
//
// The single-statement form — CTE, UPDATE, final SELECT — makes the update
// atomic with the cursor read: max(pk) and rows_updated come back from the
// query that did the work, so a pod crash commits both or neither.
//
// Single-PK only. codegen.buildPhaseSplit emits no BackfillField for a
// composite-PK entity, and BeginBackfillPlan refuses a plan whose field list is
// empty for that reason, so this template never sees one.
func ChunkSQL(qualifiedTable, pkColumn, field, expression string) string {
	return fmt.Sprintf(`WITH chunk AS (
    SELECT %[2]s FROM %[1]s
    WHERE %[3]s IS NULL AND %[2]s > $1
    ORDER BY %[2]s
    LIMIT $2
),
updated AS (
    UPDATE %[1]s t
    SET %[3]s = %[4]s
    FROM chunk
    WHERE t.%[2]s = chunk.%[2]s
    RETURNING t.%[2]s
)
SELECT COALESCE(MAX(%[2]s), $1) AS new_last_pk, COUNT(*) AS rows_updated FROM updated`,
		qualifiedTable,
		quoteIdent(pkColumn),
		quoteIdent(field),
		expression,
	)
}

// quoteIdent mirrors codegen/sql.go's quoteIdent: double-quotes the identifier,
// doubling any quote inside it. A widening of the grammar cannot then carry a
// reserved word through.
func quoteIdent(s string) string {
	if !strings.ContainsAny(s, `"`+"\n") {
		return `"` + s + `"`
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
