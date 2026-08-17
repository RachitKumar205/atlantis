package query

import (
	"strings"
	"testing"

	commonv1 "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/common/v1"
)

// The SQL shape, per width.
//
// The float4 arm casts its placeholder and the float8 arm does not. Asserting
// the strings is what makes the difference visible in review; the PG test in
// float_predicate_pg_test.go is what proves the difference matters.
func TestFloatPredicate_SQLShape(t *testing.T) {
	cases := []struct {
		name  string
		field string
		pred  *commonv1.FloatPredicate
		dpred *commonv1.DoublePredicate
		want  string
	}{
		// The outer parens are TranslateFilter's, not the arm's — a single
		// predicate is still wrapped as a one-element conjunction. Matching the
		// convention the numeric tests already use.
		{
			name: "real gt casts the placeholder", field: "score",
			pred: &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Gt{Gt: 1.5}},
			want: "(score > $1::real)",
		},
		{
			name: "real eq casts the placeholder", field: "score",
			pred: &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_Eq{Eq: 0.1}},
			want: "(score = $1::real)",
		},
		{
			name: "real is_null needs no placeholder", field: "score",
			pred: &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_IsNull{IsNull: true}},
			want: "(score IS NULL)",
		},
		{
			name: "real in casts every placeholder", field: "score",
			pred: &commonv1.FloatPredicate{Op: &commonv1.FloatPredicate_In{
				In: &commonv1.FloatList{Values: []float32{1, 2}}}},
			want: "(score IN ($1::real, $2::real))",
		},
		{
			name: "double gt does not cast", field: "weight",
			dpred: &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Gt{Gt: 1.5}},
			want:  "(weight > $1)",
		},
		{
			name: "double lte does not cast", field: "weight",
			dpred: &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_Lte{Lte: 9}},
			want:  "(weight <= $1)",
		},
		{
			name: "double in does not cast", field: "weight",
			dpred: &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_In{
				In: &commonv1.DoubleList{Values: []float64{1, 2}}}},
			want: "(weight IN ($1, $2))",
		},
		{
			name: "double is_not_null needs no placeholder", field: "weight",
			dpred: &commonv1.DoublePredicate{Op: &commonv1.DoublePredicate_IsNotNull{IsNotNull: true}},
			want:  "(weight IS NOT NULL)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFilter(t)
			if tc.pred != nil {
				setPredicate(t, f, tc.field, tc.pred)
			} else {
				setPredicate(t, f, tc.field, tc.dpred)
			}
			sql, _ := translate(t, f)
			if sql != tc.want {
				t.Errorf("got %q, want %q", sql, tc.want)
			}
		})
	}
}

// A float4 arm must bind a float32, not a float64.
//
// The SQL cast alone would still produce the right answer — PG would round the
// float8 parameter down at the database. Binding the narrow value means the
// argument and the cast describe the same number, so the two cannot drift
// apart, and a reader of the args slice sees the width the column has.
func TestFloatPredicate_BindsTheColumnsWidth(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "score", &commonv1.FloatPredicate{
		Op: &commonv1.FloatPredicate_Gt{Gt: 0.1}})
	_, args := translate(t, f)
	if len(args) != 1 {
		t.Fatalf("got %d args, want 1", len(args))
	}
	if _, ok := args[0].(float32); !ok {
		t.Errorf("a `real` column bound %T; float4 comparisons should carry "+
			"float32 so the bound value and the ::real cast agree", args[0])
	}

	f2 := newFilter(t)
	setPredicate(t, f2, "weight", &commonv1.DoublePredicate{
		Op: &commonv1.DoublePredicate_Gt{Gt: 0.1}})
	_, args2 := translate(t, f2)
	if len(args2) != 1 {
		t.Fatalf("got %d args, want 1", len(args2))
	}
	if _, ok := args2[0].(float64); !ok {
		t.Errorf("a `double` column bound %T, want float64", args2[0])
	}
}

// An oversized IN list is refused rather than sent.
//
// translateNumericPredicate does not enforce this and should; the float arm
// does from the start rather than inheriting the omission.
func TestFloatPredicate_RejectsAnOversizedInList(t *testing.T) {
	vals := make([]float32, MaxInListSize+1)
	f := newFilter(t)
	setPredicate(t, f, "score", &commonv1.FloatPredicate{
		Op: &commonv1.FloatPredicate_In{In: &commonv1.FloatList{Values: vals}}})
	err := translateErr(t, f)
	if err == nil {
		t.Fatalf("a list of %d was accepted; the cap is %d", len(vals), MaxInListSize)
	}
	if !strings.Contains(err.Error(), "in list exceeds") {
		t.Errorf("error %q does not name the cap", err)
	}
}

// An empty IN list contributes no predicate, matching every other kind.
func TestFloatPredicate_EmptyInListDropsOut(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "score", &commonv1.FloatPredicate{
		Op: &commonv1.FloatPredicate_In{In: &commonv1.FloatList{}}})
	sql, args := translate(t, f)
	if sql != "" || len(args) != 0 {
		t.Errorf("empty IN produced sql=%q args=%v, want no contribution", sql, args)
	}
}
