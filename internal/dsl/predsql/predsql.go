// Package predsql renders a resolved partial-index predicate (dsl.PredExpr) to
// SQL and to a stable diff-identity key. It is a leaf shared by two callers that
// must not depend on each other: codegen (emits CREATE INDEX ... WHERE) and
// introspect (renders the declared predicate so the drift matcher can compare it
// against the live one through pg_query). Keeping a single renderer here is what
// guarantees the emitted predicate and the matched predicate never diverge.
//
// A PredExpr is one of: PredKindNull / PredKindCompare (the two legacy shapes,
// kept structured so their SQL / diff key / cache suffix stay byte-identical) or
// PredKindExpr (any other predicate, already canonical SQL text from pg_query).
package predsql

import (
	"strconv"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Render returns the SQL boolean expression for a predicate, with no leading
// WHERE.
func Render(p *dsl.PredExpr) string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case dsl.PredKindExpr:
		return p.Text
	case dsl.PredKindCompare:
		return renderOperand(p.Left) + " " + sqlOp(p.Op) + " " + renderOperand(p.Right)
	case dsl.PredKindNull:
		if p.Negated {
			return renderOperand(p.Arg) + " IS NOT NULL"
		}
		return renderOperand(p.Arg) + " IS NULL"
	}
	return ""
}

func renderOperand(o *dsl.PredOperand) string {
	if o == nil {
		return ""
	}
	switch o.Kind {
	case dsl.OperandColumn:
		return schema.QuoteIdent(o.Name)
	case dsl.OperandLiteral:
		if o.Literal != nil {
			return schema.DefaultExpr(*o.Literal)
		}
	}
	return ""
}

// sqlOp emits the Postgres-canonical operator (`!=` deparses as `<>`).
func sqlOp(op string) string {
	if op == "!=" {
		return "<>"
	}
	return op
}

// CanonicalKey returns the predicate portion of an index's diff-identity key,
// prefixed with `|`. For the two legacy shapes it reproduces the exact bytes the
// diff engine produced before the predicate became a tree, so an already-applied
// index never re-diffs as drop+recreate. PredKindExpr uses its canonical text
// (which begins with neither a column name in the legacy layout nor `|`, so it
// cannot collide with a legacy key).
func CanonicalKey(p *dsl.PredExpr) string {
	if p == nil {
		return ""
	}
	if field, op, isNull, lit, ok := p.LegacyForm(); ok {
		if op == "" {
			if isNull {
				return "|" + field + " is null"
			}
			return "|" + field + " is not null"
		}
		return "|" + field + " " + op + legacyLiteralTag(lit)
	}
	return "|" + p.Text
}

// legacyLiteralTag reproduces the diff engine's pre-tree literal encoding
// (" s:"/" i:"/" b:") exactly. Only string/int/bool ever reach here (LegacyForm
// excludes float literals).
func legacyLiteralTag(d *dsl.Default) string {
	if d == nil {
		return ""
	}
	switch d.Kind {
	case dsl.DefaultIRString:
		return " s:" + d.Str
	case dsl.DefaultIRInt:
		return " i:" + strconv.FormatInt(d.Int, 10)
	case dsl.DefaultIRBool:
		return " b:" + strconv.FormatBool(d.Bool)
	}
	return ""
}
