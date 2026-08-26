//go:build cgo

package dsl

import (
	"fmt"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// lowerPredicate parses and validates a partial-index `where` predicate captured
// verbatim by the lexer.
//
// The predicate is a SQL boolean expression, so it goes to Postgres's own
// parser: DSL `"..."` literals become SQL `'...'`, then pg_query parses the
// result. That covers the legal index-predicate surface without a second
// grammar.
//
// Two shapes — `<col> IS [NOT] NULL` and `<col> <op> <literal>` — are detected
// from the tree and returned as the structured PredExpr, keeping their IR JSON,
// diff key and cache suffix byte-identical. Everything else returns
// PredKindExpr holding the canonical text and its referenced columns.
//
// Only what a raw parse can see is rejected here: empty or unparseable
// predicates, multi-statement injection, subqueries, and window/aggregate
// syntax. validateEntity checks column existence later through
// PredExpr.Columns(), and Postgres rejects plain aggregates and volatile
// functions at CREATE INDEX time.
func lowerPredicate(raw string) (*PredExpr, error) {
	sql := dslToSQL(raw)
	if sql == "" {
		return nil, fmt.Errorf("partial index: empty where predicate")
	}
	tree, err := pg.Parse("SELECT 1 WHERE " + sql)
	if err != nil {
		return nil, fmt.Errorf("partial index: invalid where predicate %q: %w", sql, err)
	}
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("partial index: where predicate must be a single expression, not %d statements", len(tree.Stmts))
	}
	sel := tree.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil || sel.WhereClause == nil {
		return nil, fmt.Errorf("partial index: could not parse where predicate %q", sql)
	}
	where := sel.WhereClause

	var cols []string
	if err := walkPredicate(where, &cols); err != nil {
		return nil, fmt.Errorf("partial index: %w", err)
	}

	if pe, ok := detectLegacyShape(where); ok {
		return pe, nil
	}
	return &PredExpr{Kind: PredKindExpr, Text: sql, Cols: dedupeCols(cols)}, nil
}

// walkPredicate recursively rejects the constructs detectable in a raw parse
// (subqueries, window functions, aggregate syntax) and collects every column
// reference. Unrecognized nodes are skipped — Postgres is the backstop at apply.
func walkPredicate(n *pg.Node, cols *[]string) error {
	if n == nil {
		return nil
	}
	if sl := n.GetSubLink(); sl != nil {
		return fmt.Errorf("where predicate cannot contain a subquery")
	}
	if fc := n.GetFuncCall(); fc != nil {
		if fc.Over != nil {
			return fmt.Errorf("where predicate cannot contain a window function")
		}
		if fc.AggStar || fc.AggDistinct || fc.AggWithinGroup || len(fc.AggOrder) > 0 || fc.AggFilter != nil {
			return fmt.Errorf("where predicate cannot contain an aggregate")
		}
		for _, a := range fc.Args {
			if err := walkPredicate(a, cols); err != nil {
				return err
			}
		}
		return nil
	}
	if cr := n.GetColumnRef(); cr != nil {
		if name, ok := columnRefName(cr); ok {
			*cols = append(*cols, name)
		}
		return nil
	}
	for _, child := range predChildren(n) {
		if err := walkPredicate(child, cols); err != nil {
			return err
		}
	}
	return nil
}

// predChildren returns the child nodes of the expression node kinds that can
// appear in an index predicate, so walkPredicate can recurse without reflection.
func predChildren(n *pg.Node) []*pg.Node {
	switch {
	case n.GetBoolExpr() != nil:
		return n.GetBoolExpr().Args
	case n.GetAExpr() != nil:
		ax := n.GetAExpr()
		return []*pg.Node{ax.Lexpr, ax.Rexpr}
	case n.GetNullTest() != nil:
		return []*pg.Node{n.GetNullTest().Arg}
	case n.GetBooleanTest() != nil:
		return []*pg.Node{n.GetBooleanTest().Arg}
	case n.GetTypeCast() != nil:
		return []*pg.Node{n.GetTypeCast().Arg}
	case n.GetCoalesceExpr() != nil:
		return n.GetCoalesceExpr().Args
	case n.GetMinMaxExpr() != nil:
		return n.GetMinMaxExpr().Args
	case n.GetAArrayExpr() != nil:
		return n.GetAArrayExpr().Elements
	case n.GetList() != nil:
		return n.GetList().Items
	case n.GetCaseExpr() != nil:
		ce := n.GetCaseExpr()
		out := []*pg.Node{ce.Arg, ce.Defresult}
		out = append(out, ce.Args...)
		return out
	case n.GetCaseWhen() != nil:
		cw := n.GetCaseWhen()
		return []*pg.Node{cw.Expr, cw.Result}
	case n.GetSubLink() != nil:
		return []*pg.Node{n.GetSubLink().Testexpr}
	}
	return nil
}

// detectLegacyShape returns the structured legacy PredExpr when the WHERE node is
// exactly one of the two pre-tree shapes, so its serialization stays
// byte-identical. ok=false for everything else (→ PredKindExpr text).
func detectLegacyShape(where *pg.Node) (*PredExpr, bool) {
	// <col> IS [NOT] NULL
	if nt := where.GetNullTest(); nt != nil {
		if name, ok := columnRefName(nt.Arg.GetColumnRef()); ok {
			return &PredExpr{
				Kind:    PredKindNull,
				Arg:     &PredOperand{Kind: OperandColumn, Name: name},
				Negated: nt.Nulltesttype == pg.NullTestType_IS_NOT_NULL,
			}, true
		}
		return nil, false
	}
	// <col> <op> <string|int|bool literal>
	if ax := where.GetAExpr(); ax != nil && ax.Kind == pg.A_Expr_Kind_AEXPR_OP {
		op, ok := legacyOpFromPG(aExprOpName(ax))
		if !ok {
			return nil, false
		}
		name, okc := columnRefName(ax.Lexpr.GetColumnRef())
		if !okc {
			return nil, false
		}
		lit, okl := legacyLiteral(ax.Rexpr.GetAConst())
		if !okl {
			return nil, false
		}
		return &PredExpr{
			Kind:  PredKindCompare,
			Op:    op,
			Left:  &PredOperand{Kind: OperandColumn, Name: name},
			Right: &PredOperand{Kind: OperandLiteral, Literal: lit},
		}, true
	}
	return nil, false
}

// legacyOpFromPG maps a parsed operator name to the DSL spelling the pre-tree
// parser used (`<>` was written `!=`). Only the six legacy operators qualify.
func legacyOpFromPG(op string) (string, bool) {
	switch op {
	case "=", "<", "<=", ">", ">=":
		return op, true
	case "<>", "!=":
		return "!=", true
	}
	return "", false
}

// legacyLiteral extracts a string/int/bool constant into a Default, matching the
// pre-tree serialization. Float and any other const → not a legacy literal.
func legacyLiteral(c *pg.A_Const) (*Default, bool) {
	if c == nil || c.Isnull {
		return nil, false
	}
	switch v := c.Val.(type) {
	case *pg.A_Const_Sval:
		return &Default{Kind: DefaultIRString, Str: v.Sval.Sval}, true
	case *pg.A_Const_Ival:
		return &Default{Kind: DefaultIRInt, Int: int64(v.Ival.Ival)}, true
	case *pg.A_Const_Boolval:
		return &Default{Kind: DefaultIRBool, Bool: v.Boolval.Boolval}, true
	}
	return nil, false
}

func columnRefName(cr *pg.ColumnRef) (string, bool) {
	if cr == nil || len(cr.Fields) != 1 {
		return "", false
	}
	s := cr.Fields[0].GetString_()
	if s == nil {
		return "", false
	}
	return s.Sval, true
}

func aExprOpName(ax *pg.A_Expr) string {
	if len(ax.Name) == 0 {
		return ""
	}
	s := ax.Name[len(ax.Name)-1].GetString_()
	if s == nil {
		return ""
	}
	return s.Sval
}
