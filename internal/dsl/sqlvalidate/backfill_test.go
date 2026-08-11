package sqlvalidate

import (
	"strings"
	"testing"
)

func cols(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestBackfill_Concat_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`first_name || ' ' || last_name`,
		cols("first_name", "last_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestBackfill_Coalesce_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`coalesce(first_name, 'unknown')`,
		cols("first_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("expected ok, got %v", err)
	}
}

func TestBackfill_TypeCast_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`text_col::int + 1`,
		cols("text_col"),
		"int_col",
	)
	if err != nil {
		t.Errorf("type cast should be allowed: %v", err)
	}
}

func TestBackfill_CaseExpression_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`case when active then 'yes' else 'no' end`,
		cols("active"),
		"label",
	)
	if err != nil {
		t.Errorf("case expression should be allowed: %v", err)
	}
}

func TestBackfill_EmptyExpr_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(``, cols(), "x")
	if err == nil {
		t.Errorf("empty expression should be rejected")
	}
}

func TestBackfill_ParseFailure_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(`!!! not sql !!!`, cols(), "x")
	if err == nil {
		t.Errorf("garbage SQL should be rejected at parse time")
	}
}

func TestBackfill_Subquery_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`(SELECT password FROM secrets WHERE id = 1)`,
		cols("password"),
		"derived",
	)
	if err == nil || !strings.Contains(err.Error(), "subquer") {
		t.Errorf("subquery should be rejected with a subquery error, got %v", err)
	}
}

func TestBackfill_Nextval_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`nextval('users_id_seq')`,
		cols(),
		"id",
	)
	if err == nil || !strings.Contains(err.Error(), "nextval") {
		t.Errorf("nextval should be rejected by whitelist, got %v", err)
	}
}

func TestBackfill_CurrentSetting_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`current_setting('app.something')`,
		cols(),
		"x",
	)
	if err == nil || !strings.Contains(err.Error(), "current_setting") {
		t.Errorf("current_setting should be rejected, got %v", err)
	}
}

func TestBackfill_PgReadFile_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`pg_read_file('/etc/passwd')`,
		cols(),
		"x",
	)
	if err == nil || !strings.Contains(err.Error(), "pg_read_file") {
		t.Errorf("pg_read_file should be rejected, got %v", err)
	}
}

func TestBackfill_UnknownColumn_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`first_name || ' ' || last_nam`,
		cols("first_name", "last_name"),
		"display_name",
	)
	if err == nil || !strings.Contains(err.Error(), "last_nam") {
		t.Errorf("unknown column should be rejected, got %v", err)
	}
}

func TestBackfill_SelfReference_Rejected(t *testing.T) {
	err := ValidateBackfillExpression(
		`display_name || '!'`,
		cols("display_name"),
		"display_name",
	)
	if err == nil || !strings.Contains(err.Error(), "display_name") {
		t.Errorf("self-reference should be rejected, got %v", err)
	}
}

func TestBackfill_NestedFuncCall_DeepCheck(t *testing.T) {
	err := ValidateBackfillExpression(
		`coalesce(lower(first_name), 'x')`,
		cols("first_name"),
		"display_name",
	)
	if err != nil {
		t.Errorf("nested whitelisted calls should be ok: %v", err)
	}
}

func TestBackfill_NestedUnsafeFuncCall_Rejected(t *testing.T) {
	// Outer call is whitelisted but the nested one isn't — must still be rejected.
	err := ValidateBackfillExpression(
		`coalesce(nextval('s'), 0)`,
		cols(),
		"id",
	)
	if err == nil || !strings.Contains(err.Error(), "nextval") {
		t.Errorf("nested unsafe call should be rejected, got %v", err)
	}
}

func TestBackfill_NullTestAndArithmetic_OK(t *testing.T) {
	err := ValidateBackfillExpression(
		`case when a is null then 0 else a + b end`,
		cols("a", "b"),
		"sum_or_zero",
	)
	if err != nil {
		t.Errorf("null test + arithmetic should be ok: %v", err)
	}
}

// A backfill expression is spliced verbatim into a live UPDATE, so it is one of
// the seven caller-authored SQL surfaces.
//
// The cases below are the ones that DISTINGUISH the exhaustive pass from the
// hand-written walk it was added ahead of. An earlier version of this test used
// BoolExpr, NullTest, CaseExpr, A_Expr, TypeCast and CoalesceExpr — all of which
// the old walk already descended into, so every case passed against the code
// this test was written to replace. It demonstrated nothing.
//
// Each shape here hosts a FuncCall in a node the old walk had no case for, and
// each was measured accepted by it and rejected now.
func TestValidateBackfillExpression_RejectsCallsTheOldWalkMissed(t *testing.T) {
	cols := map[string]bool{"status": true, "total": true}
	const evil = `set_config('atlantis.tenant','victim',true)`

	for _, tc := range []struct{ name, expr string }{
		{"greatest (MinMaxExpr)", `greatest(` + evil + `, 'x')`},
		{"least (MinMaxExpr)", `least(` + evil + `, 'x')`},
		{"COLLATE (CollateClause)", evil + `::text COLLATE "C"`},
		{"grouping (GroupingFunc)", `grouping(` + evil + `)`},
		{"array subscript (A_Indirection)", `(ARRAY[` + evil + `])[1]`},
		{"xmlelement (XmlExpr)", `xmlelement(name foo, ` + evil + `)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBackfillExpression(tc.expr, cols, ""); err == nil {
				t.Errorf("accepted a backfill expression that rebinds the tenant. "+
					"internal/backfill/splicer.go splices this verbatim into "+
					"`UPDATE ... SET <col> = <expr>` against live rows:\n  %s", tc.expr)
			}
		})
	}
}

// And the shapes the old walk did reach, kept so the pass is not a regression
// in the other direction.
func TestValidateBackfillExpression_RejectsHiddenCalls(t *testing.T) {
	cols := map[string]bool{"status": true, "total": true}

	for _, tc := range []struct {
		name, expr string
	}{
		{"direct", `set_config('atlantis.tenant','victim',true)`},
		{"the setter itself", `atlantis.set_partition('victim')`},
		{"behind AND", `status IS NOT NULL AND set_config('atlantis.tenant','victim',true) IS NOT NULL`},
		{"in a CASE", `CASE WHEN true THEN set_config('atlantis.tenant','victim',true) ELSE status END`},
		{"nested two deep", `coalesce(nullif(set_config('atlantis.tenant','victim',true),''),'x')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateBackfillExpression(tc.expr, cols, ""); err == nil {
				t.Errorf("accepted a backfill expression that rebinds the tenant:\n  %s", tc.expr)
			}
		})
	}
}

// Ordinary backfill expressions must still pass.
func TestValidateBackfillExpression_AcceptsOrdinary(t *testing.T) {
	cols := map[string]bool{"status": true, "total": true}
	for _, expr := range []string{
		`'draft'`,
		`0`,
		`coalesce(status, 'draft')`,
		`total * 2`,
		`lower(status)`,
		`nullif(status, '')`,
	} {
		if err := ValidateBackfillExpression(expr, cols, ""); err != nil {
			t.Errorf("rejected an ordinary backfill expression %q: %v", expr, err)
		}
	}
}

// The expression is spliced into `SET <col> = <expr>` with no wrapper, so an
// assignment-level comma ends its own assignment and starts another.
//
// The probe used to be `SELECT (<expr>)`, whose parentheses turned these into
// row constructors and accepted them. Executed against a live table with the
// real ChunkSQL, the first case below produced
//
//	SET "nickname" = 'n', tenant = 'attacker', secret = 'clobbered'
//
// and rewrote the tenant discriminator of every row. A backfill that reads as
// "populate the new nickname column" moved rows between tenants at apply time.
func TestBackfill_AssignmentEscape_Rejected(t *testing.T) {
	cols := map[string]bool{"email": true, "nickname": true, "tenant": true}
	for _, expr := range []string{
		`'n', tenant = 'attacker'`,
		`'n', tenant = 'attacker', secret = 'clobbered'`,
		`email, tenant = current_setting('x')`,
	} {
		if err := ValidateBackfillExpression(expr, cols, "nickname"); err == nil {
			t.Errorf("%q was accepted. Spliced into SET it assigns to columns the "+
				"backfill does not own, including the tenant discriminator", expr)
		}
	}
}

// Clauses reachable from the right-hand side that change which rows are written.
func TestBackfill_TrailingClause_Rejected(t *testing.T) {
	cols := map[string]bool{"email": true, "nickname": true}
	for _, expr := range []string{
		`'n' WHERE true`,
		`'n' RETURNING *`,
		`'n' FROM other_table`,
	} {
		if err := ValidateBackfillExpression(expr, cols, "nickname"); err == nil {
			t.Errorf("%q was accepted. The chunked UPDATE supplies its own WHERE "+
				"and FROM; an added one changes the row set", expr)
		}
	}
}

// A subquery reached through a node the hand-written walk had no case for.
//
// Each of these was ACCEPTED while the function check beside them was already
// exhaustive: MinMaxExpr, A_Indirection, CollateClause and BooleanTest were
// simply missing from the permit-list. Read any table, write it into a column.
func TestBackfill_SubqueryUnderUnwalkedNode_Rejected(t *testing.T) {
	cols := map[string]bool{"email": true, "nickname": true}
	for _, expr := range []string{
		`GREATEST('a', (SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))`,
		`LEAST('a', (SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))`,
		`((SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1))[1]`,
		`(SELECT ir::text FROM atlantis.ir_checkpoint LIMIT 1) COLLATE "C"`,
		`((SELECT true) IS TRUE)::text`,
	} {
		err := ValidateBackfillExpression(expr, cols, "nickname")
		if err == nil {
			t.Errorf("%q was accepted. It reads a table the backfill was never "+
				"granted and writes the result into a column, in a live UPDATE", expr)
			continue
		}
		if !strings.Contains(err.Error(), "subquer") {
			t.Errorf("%q was rejected for the wrong reason (%v); the subquery is "+
				"the finding, and a function-name rejection would not survive "+
				"renaming the function", expr, err)
		}
	}
}

// Ordinary expressions must still pass. A gate that refuses everything stops a
// healthy deployment just as surely as one that refuses nothing lets it leak.
func TestBackfill_LegitimateExpressions_Accepted(t *testing.T) {
	cols := map[string]bool{"email": true, "first": true, "last": true, "nickname": true}
	for _, expr := range []string{
		`'unknown'`,
		`lower(email)`,
		`coalesce(first, 'x')`,
		`first || ' ' || last`,
		`CASE WHEN email IS NULL THEN 'none' ELSE lower(email) END`,
		`substr(email, 1, 10)`,
	} {
		if err := ValidateBackfillExpression(expr, cols, "nickname"); err != nil {
			t.Errorf("%q was rejected: %v. This is an ordinary backfill", expr, err)
		}
	}
}
