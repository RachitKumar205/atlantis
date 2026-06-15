package introspect

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func TestPredMatchesLive(t *testing.T) {
	isNull := func(field string, isNull bool) *dsl.PartialPred {
		return &dsl.PartialPred{Field: field, IsNull: isNull}
	}
	cmp := func(field, op string, lit *dsl.Default) *dsl.PartialPred {
		return &dsl.PartialPred{Field: field, Op: op, Literal: lit}
	}
	str := func(s string) *dsl.Default { return &dsl.Default{Kind: dsl.DefaultIRString, Str: s} }
	num := func(i int64) *dsl.Default { return &dsl.Default{Kind: dsl.DefaultIRInt, Int: i} }

	cases := []struct {
		name     string
		declared *dsl.PartialPred
		live     string
		want     bool
	}{
		{"is null", isNull("deleted_at", true), "(deleted_at IS NULL)", true},
		{"is not null", isNull("deleted_at", false), "(deleted_at IS NOT NULL)", true},
		{"string eq, text cast", cmp("status", "=", str("active")), "(status = 'active'::text)", true},
		{"string eq, varchar cast (space in type)", cmp("status", "=", str("active")), "(status = 'active'::character varying)", true},
		{"int gt", cmp("tier", ">", num(3)), "(tier > 3)", true},
		{"!= normalizes to <>", cmp("status", "!=", str("x")), "(status <> 'x'::text)", true},
		{"quoted reserved identifier", cmp("order", "=", num(1)), `("order" = 1)`, true},

		{"null-ness mismatch", isNull("deleted_at", true), "(deleted_at IS NOT NULL)", false},
		{"int value mismatch", cmp("tier", ">", num(3)), "(tier > 4)", false},
		{"string value mismatch", cmp("status", "=", str("active")), "(status = 'inactive'::text)", false},
		{"operator mismatch", cmp("tier", ">", num(3)), "(tier >= 3)", false},
		{"field mismatch", isNull("deleted_at", true), "(removed_at IS NULL)", false},
		{"compound not matched (safe)", isNull("deleted_at", true), "(deleted_at IS NULL AND tier > 3)", false},
		{"unparseable (safe)", isNull("deleted_at", true), "garbage ((", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := predMatchesLive(tc.declared, tc.live); got != tc.want {
				t.Errorf("predMatchesLive = %v, want %v (declared %+v, live %q)", got, tc.want, tc.declared, tc.live)
			}
		})
	}
}
