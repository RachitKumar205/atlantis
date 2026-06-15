package dsl

import (
	"fmt"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// lowerPredicate parses and validates a partial-index `where` predicate captured
// verbatim by the lexer. The predicate is a SQL boolean expression — atlantis
// does NOT reimplement Postgres's grammar; it transforms DSL `"..."` string
// literals to SQL `'...'`, then parses with Postgres's own parser (pg_query),
// the same delegate-to-the-authority approach as the drift matcher. This covers
// the full legal index-predicate surface by construction.
//
// The two pre-tree shapes (`<col> IS [NOT] NULL`, `<col> <op> <string|int|bool
// literal>`) are detected from the parse tree and returned as the structured
// legacy PredExpr so their IR JSON / diff key / cache suffix stay byte-identical
// (no checkpoint churn). Everything else is returned as PredKindExpr holding the
// canonical predicate text plus its referenced columns.
//
// Column existence is validated later (validateEntity) via PredExpr.Columns();
// here we only reject what is detectable in a raw parse — empty/unparseable
// predicates, multi-statement injection, subqueries, and window/aggregate
// syntax. Plain aggregates and volatile functions are rejected by Postgres at
// CREATE INDEX (apply) time, exactly as volatility has always been.
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

// dslToSQL rewrites a captured predicate into SQL: DSL `"..."` string literals
// become SQL `'...'` (decoding DSL escapes, doubling embedded single quotes),
// `/* */` block comments are dropped, and runs of whitespace collapse to a
// single space. SQL `'...'` strings are copied verbatim, and `"`/`'`/comment
// markers inside the other kind of quote are inert (DSL-string state takes
// precedence). The DSL has no quoted identifiers, so a `"..."` is always a
// string.
func dslToSQL(raw string) string {
	var b strings.Builder
	pendingSpace := false
	emit := func(s string) {
		if s == "" {
			return
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(s)
	}

	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '"':
			j := i + 1
			var s strings.Builder
			for j < n && raw[j] != '"' {
				if raw[j] == '\\' && j+1 < n {
					switch raw[j+1] {
					case 'n':
						s.WriteByte('\n')
					case 't':
						s.WriteByte('\t')
					default:
						s.WriteByte(raw[j+1])
					}
					j += 2
					continue
				}
				s.WriteByte(raw[j])
				j++
			}
			if j < n {
				j++ // closing quote
			}
			emit("'" + strings.ReplaceAll(s.String(), "'", "''") + "'")
			i = j
		case c == '\'':
			j := i + 1
			for j < n {
				if raw[j] == '\'' {
					if j+1 < n && raw[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			emit(raw[i:j])
			i = j
		case c == '/' && i+1 < n && raw[i+1] == '*':
			j := i + 2
			for j+1 < n && !(raw[j] == '*' && raw[j+1] == '/') {
				j++
			}
			j += 2
			if j > n {
				j = n
			}
			pendingSpace = true
			i = j
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pendingSpace = true
			i++
		default:
			emit(string(c))
			i++
		}
	}
	return strings.TrimSpace(b.String())
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

func dedupeCols(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
