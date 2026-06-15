package introspect

import "github.com/rachitkumar205/atlantis/internal/dsl"

// Predicate builders shared by the drift tests. A PredExpr is either a legacy
// shape (null / column-op-literal) or PredKindExpr holding canonical SQL text.
// The matcher is the DB normalizer (normalize.go), verified end-to-end against a
// live Postgres in normalize_live_test.go; the pure-Go unit tests inject a fake.

func column(name string) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandColumn, Name: name}
}
func strLit(s string) *dsl.Default { return &dsl.Default{Kind: dsl.DefaultIRString, Str: s} }
func intLit(i int64) *dsl.Default  { return &dsl.Default{Kind: dsl.DefaultIRInt, Int: i} }

func nullPred(name string, negated bool) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindNull, Arg: column(name), Negated: negated}
}
func cmpPred(op, colName string, lit *dsl.Default) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindCompare, Op: op, Left: column(colName),
		Right: &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: lit}}
}

// exprPred builds a PredKindExpr from canonical SQL text (single-quote strings,
// the post-transform form) and its referenced columns.
func exprPred(text string, cols ...string) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindExpr, Text: text, Cols: cols}
}

// fakeNormalize stands in for the Postgres normalizer in pure-Go drift tests: it
// deparses the simple null shapes the way pg_get_expr would, so the classify
// logic runs without a database. Real deparse fidelity is covered by
// normalize_live_test.go.
func fakeNormalize(_ string, p *dsl.PredExpr) (string, bool) {
	if p.Kind == dsl.PredKindNull && p.Arg != nil && p.Arg.Kind == dsl.OperandColumn {
		if p.Negated {
			return "(" + p.Arg.Name + " IS NOT NULL)", true
		}
		return "(" + p.Arg.Name + " IS NULL)", true
	}
	return "", false
}
