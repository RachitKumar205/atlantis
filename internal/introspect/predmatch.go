package introspect

import (
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// predMatchesLive reports whether a declared partial-index predicate is
// equivalent to the live `pg_get_expr(indpred)` text of a partial unique
// index. Both are parsed with pg_query so Postgres's canonicalization —
// outer parens, typed casts (`'x'::character varying`), identifier quoting,
// and `!=`/`<>` — never causes a spurious mismatch.
//
// The declared grammar is restricted to two shapes (`<field> IS [NOT] NULL`
// and `<field> <op> <literal>`), so the matcher only recognizes those. A
// live predicate that doesn't reduce to the declared shape returns false —
// i.e. it stays drift. That is the safe direction: the matcher never
// silently accepts an undeclared uniqueness, it only refuses to recognize a
// declared one (which surfaces as a drift the operator resolves).
func predMatchesLive(declared *dsl.PartialPred, liveText string) bool {
	if declared == nil || strings.TrimSpace(liveText) == "" {
		return false
	}
	where := parseWhereExpr(liveText)
	if where == nil {
		return false
	}

	// `<field> IS [NOT] NULL`
	if declared.Op == "" {
		nt := where.GetNullTest()
		if nt == nil {
			return false
		}
		col, ok := columnRefName(nt.Arg)
		if !ok || !strings.EqualFold(col, declared.Field) {
			return false
		}
		gotIsNull := nt.Nulltesttype == pg.NullTestType_IS_NULL
		return gotIsNull == declared.IsNull
	}

	// `<field> <op> <literal>`
	ax := where.GetAExpr()
	if ax == nil || ax.Kind != pg.A_Expr_Kind_AEXPR_OP {
		return false
	}
	if !opEquivalent(aExprOp(ax), declared.Op) {
		return false
	}
	col, ok := columnRefName(ax.Lexpr)
	if !ok || !strings.EqualFold(col, declared.Field) {
		return false
	}
	return constEqualsDefault(stripCasts(ax.Rexpr), declared.Literal)
}

// parseWhereExpr parses a bare predicate by wrapping it in a trivial SELECT
// and returning the WHERE node, or nil if it doesn't parse.
func parseWhereExpr(text string) *pg.Node {
	tree, err := pg.Parse("SELECT 1 WHERE " + text)
	if err != nil || len(tree.Stmts) != 1 {
		return nil
	}
	sel := tree.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil {
		return nil
	}
	return sel.WhereClause
}

// stripCasts unwraps any nested TypeCast wrappers (e.g. `'x'::text`) so the
// inner A_Const is compared, not its Postgres-added coercion.
func stripCasts(n *pg.Node) *pg.Node {
	for n != nil {
		tc := n.GetTypeCast()
		if tc == nil {
			return n
		}
		n = tc.Arg
	}
	return n
}

// columnRefName returns the (last) identifier of a ColumnRef node. pg_query
// stores the actual identifier, so quoting/case is handled for us.
func columnRefName(n *pg.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	cr := n.GetColumnRef()
	if cr == nil || len(cr.Fields) == 0 {
		return "", false
	}
	last := cr.Fields[len(cr.Fields)-1].GetString_()
	if last == nil {
		return "", false
	}
	return last.Sval, true
}

// aExprOp returns the operator name of an A_Expr (e.g. "=", "<>").
func aExprOp(ax *pg.A_Expr) string {
	if len(ax.Name) == 0 {
		return ""
	}
	s := ax.Name[len(ax.Name)-1].GetString_()
	if s == nil {
		return ""
	}
	return s.Sval
}

// opEquivalent normalizes `!=` to `<>` (the DSL writes `!=`; Postgres
// deparses `<>`).
func opEquivalent(live, declared string) bool {
	norm := func(o string) string {
		if o == "!=" {
			return "<>"
		}
		return o
	}
	return norm(live) == norm(declared)
}

// constEqualsDefault compares a live A_Const (cast already stripped) to the
// declared literal.
func constEqualsDefault(n *pg.Node, d *dsl.Default) bool {
	if n == nil || d == nil {
		return false
	}
	c := n.GetAConst()
	if c == nil || c.Isnull {
		return false
	}
	switch d.Kind {
	case dsl.DefaultIRString:
		if v, ok := c.Val.(*pg.A_Const_Sval); ok {
			return v.Sval.Sval == d.Str
		}
	case dsl.DefaultIRInt:
		if v, ok := c.Val.(*pg.A_Const_Ival); ok {
			return int64(v.Ival.Ival) == d.Int
		}
	case dsl.DefaultIRBool:
		if v, ok := c.Val.(*pg.A_Const_Boolval); ok {
			return v.Boolval.Boolval == d.Bool
		}
	}
	return false
}
