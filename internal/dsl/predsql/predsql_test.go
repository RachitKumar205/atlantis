package predsql

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func col(n string) *dsl.PredOperand { return &dsl.PredOperand{Kind: dsl.OperandColumn, Name: n} }
func lit(d *dsl.Default) *dsl.PredOperand {
	return &dsl.PredOperand{Kind: dsl.OperandLiteral, Literal: d}
}
func sval(s string) *dsl.Default { return &dsl.Default{Kind: dsl.DefaultIRString, Str: s} }
func ival(i int64) *dsl.Default  { return &dsl.Default{Kind: dsl.DefaultIRInt, Int: i} }

func null(n string, neg bool) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindNull, Arg: col(n), Negated: neg}
}
func cmp(op string, l, r *dsl.PredOperand) *dsl.PredExpr {
	return &dsl.PredExpr{Kind: dsl.PredKindCompare, Op: op, Left: l, Right: r}
}
func expr(text string) *dsl.PredExpr { return &dsl.PredExpr{Kind: dsl.PredKindExpr, Text: text} }

// TestCanonicalKey_LegacyBytes is the Sev-1 diff-key guard: the two legacy
// shapes produce the exact key bytes the diff engine emitted before predicates
// were parsed by Postgres.
func TestCanonicalKey_LegacyBytes(t *testing.T) {
	cases := []struct {
		p    *dsl.PredExpr
		want string
	}{
		{null("deleted_at", false), "|deleted_at is null"},
		{null("deleted_at", true), "|deleted_at is not null"},
		{cmp("=", col("status"), lit(sval("active"))), "|status = s:active"},
		{cmp(">=", col("tier"), lit(ival(3))), "|tier >= i:3"},
		{expr("tier between 1 and 5"), "|tier between 1 and 5"},
	}
	for _, tc := range cases {
		if got := CanonicalKey(tc.p); got != tc.want {
			t.Errorf("CanonicalKey = %q, want %q", got, tc.want)
		}
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		p    *dsl.PredExpr
		want string
	}{
		{null("deleted_at", false), `"deleted_at" IS NULL`},
		{cmp("!=", col("status"), lit(sval("x"))), `"status" <> 'x'`},
		{expr("lower(sku) like 'a%' and tier > 1"), "lower(sku) like 'a%' and tier > 1"},
	}
	for _, tc := range cases {
		if got := Render(tc.p); got != tc.want {
			t.Errorf("Render = %q, want %q", got, tc.want)
		}
	}
}
