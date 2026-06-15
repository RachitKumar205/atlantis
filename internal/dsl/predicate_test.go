package dsl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wherePred lowers an entity carrying `index partial by id where <pred>` and
// returns the resolved IR predicate.
func wherePred(t *testing.T, pred string) *PredExpr {
	t.Helper()
	ir := mustLower(t, `entity A in x {
  id     bigint primary
  status text
  tier   int
  qty    int
  price  double
  is_active boolean
  is_default boolean
  deleted_at timestamptz
  action text
  index partial by id where `+pred+`
}`)
	return ir.Entities[0].Indexes[0].Where
}

// TestLowerPredicate_LegacyShapes: the two pre-tree shapes lower to the
// structured legacy nodes so their serialization stays byte-identical.
func TestLowerPredicate_LegacyShapes(t *testing.T) {
	if p := wherePred(t, `deleted_at is null`); p.Kind != PredKindNull || p.Negated || p.Arg.Name != "deleted_at" {
		t.Errorf("is null wrong: %+v", p)
	}
	if p := wherePred(t, `deleted_at is not null`); p.Kind != PredKindNull || !p.Negated {
		t.Errorf("is not null wrong: %+v", p)
	}
	if p := wherePred(t, `status = "active"`); p.Kind != PredKindCompare || p.Op != "=" ||
		p.Left.Name != "status" || p.Right.Literal.Str != "active" {
		t.Errorf("string compare wrong: %+v", p)
	}
	if p := wherePred(t, `tier >= 3`); p.Kind != PredKindCompare || p.Op != ">=" || p.Right.Literal.Int != 3 {
		t.Errorf("int compare wrong: %+v", p)
	}
	if p := wherePred(t, `action != "x"`); p.Kind != PredKindCompare || p.Op != "!=" {
		t.Errorf("!= compare wrong: %+v", p)
	}
}

// TestLowerPredicate_FullSurface: anything beyond the two legacy shapes lowers to
// PredKindExpr holding canonical SQL text + the referenced columns.
func TestLowerPredicate_FullSurface(t *testing.T) {
	cases := []struct {
		pred string
		text string
		cols []string
	}{
		{`status = "a" and deleted_at is null`, `status = 'a' and deleted_at is null`, []string{"status", "deleted_at"}},
		{`tier between 1 and 5`, `tier between 1 and 5`, []string{"tier"}},
		{`price * qty > 0`, `price * qty > 0`, []string{"price", "qty"}},
		{`lower(status) = "x"`, `lower(status) = 'x'`, []string{"status"}},
		{`status like "a%"`, `status like 'a%'`, []string{"status"}},
		{`tier in (1, 2, 3)`, `tier in (1, 2, 3)`, []string{"tier"}},
		{`is_default`, `is_default`, []string{"is_default"}},
		{`tier::bigint > 3`, `tier::bigint > 3`, []string{"tier"}},
		{`(case when is_default then tier else 0 end) > 0`, `(case when is_default then tier else 0 end) > 0`, []string{"is_default", "tier"}},
	}
	for _, tc := range cases {
		p := wherePred(t, tc.pred)
		if p.Kind != PredKindExpr {
			t.Errorf("%q: kind = %s, want expr", tc.pred, p.Kind)
			continue
		}
		if p.Text != tc.text {
			t.Errorf("%q: text = %q, want %q", tc.pred, p.Text, tc.text)
		}
		if strings.Join(p.Cols, ",") != strings.Join(tc.cols, ",") {
			t.Errorf("%q: cols = %v, want %v", tc.pred, p.Cols, tc.cols)
		}
	}
}

func TestLowerPredicate_Errors(t *testing.T) {
	cases := []struct{ pred, want string }{
		{`tier in (select tier from other)`, "subquery"},
		{`true; drop table x`, "single expression"},
		{`nope_col is null`, "not found"}, // column validation
		{`count(*) > 0`, "aggregate"},
	}
	for _, tc := range cases {
		err := mustLowerErr(t, `entity A in x {
  id bigint primary
  tier int
  index partial by id where `+tc.pred+`
}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want contains %q", tc.pred, err, tc.want)
		}
	}
}

func TestDslToSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{`status = "active"`, `status = 'active'`},
		{`note = "it's"`, `note = 'it''s'`},
		{`x = "a\"b"`, `x = 'a"b'`},
		{`a   is    null`, `a is null`},
		{`lower(sku) like "a%" /* note */ and tier > 1`, `lower(sku) like 'a%' and tier > 1`},
		{`tag = "}" and deleted_at is null`, `tag = '}' and deleted_at is null`},
	}
	for _, tc := range cases {
		if got := dslToSQL(tc.in); got != tc.want {
			t.Errorf("dslToSQL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRealSchemaPredicates_StayLegacy parses every applied .atl under schema/
// and confirms each partial-index predicate still lexes (the raw-capture change)
// and lowers to a legacy structured shape — so no already-applied index re-diffs.
func TestRealSchemaPredicates_StayLegacy(t *testing.T) {
	root := filepath.Join("..", "..", "schema")
	if _, err := os.Stat(root); err != nil {
		t.Skip("schema/ not present")
	}
	var checked int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".atl") {
			return err
		}
		src, _ := os.ReadFile(path)
		f, perr := Parse(path, src)
		if perr != nil {
			t.Errorf("%s: parse: %v", path, perr)
			return nil
		}
		for _, decl := range f.Decls {
			e, ok := decl.(*EntityDecl)
			if !ok {
				continue
			}
			for _, m := range e.Members {
				idx, ok := m.(*IndexDecl)
				if !ok || idx.Kind != IndexPartial {
					continue
				}
				pe, lerr := lowerPredicate(idx.WhereRaw)
				if lerr != nil {
					t.Errorf("%s: predicate %q: %v", path, idx.WhereRaw, lerr)
					continue
				}
				if pe.Kind != PredKindNull && pe.Kind != PredKindCompare {
					t.Errorf("%s: applied predicate %q lowered to %s (want legacy null/compare) — would re-diff",
						path, idx.WhereRaw, pe.Kind)
				}
				checked++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checked %d applied partial-index predicates", checked)
}

// legacyRef mirrors the exact JSON of the pre-tree PartialPred struct.
type legacyRef struct {
	Field   string   `json:"field"`
	IsNull  bool     `json:"is_null,omitempty"`
	Op      string   `json:"op,omitempty"`
	Literal *Default `json:"literal,omitempty"`
}

// TestPredExprJSON_BackCompat is the Sev-1 guard: every applied predicate shape
// marshals to byte-identical JSON (so ir_checkpoint hashes never shift).
func TestPredExprJSON_BackCompat(t *testing.T) {
	cases := []struct {
		pred string
		ref  legacyRef
	}{
		{`deleted_at is null`, legacyRef{Field: "deleted_at", IsNull: true}},
		{`deleted_at is not null`, legacyRef{Field: "deleted_at"}},
		{`action = "purchased"`, legacyRef{Field: "action", Op: "=", Literal: &Default{Kind: DefaultIRString, Str: "purchased"}}},
		{`tier >= 3`, legacyRef{Field: "tier", Op: ">=", Literal: &Default{Kind: DefaultIRInt, Int: 3}}},
	}
	for _, tc := range cases {
		got, _ := json.Marshal(wherePred(t, tc.pred))
		want, _ := json.Marshal(tc.ref)
		if string(got) != string(want) {
			t.Errorf("%q marshaled to %s, want %s", tc.pred, got, want)
		}
		var back PredExpr
		if err := json.Unmarshal(want, &back); err != nil {
			t.Fatalf("unmarshal %s: %v", want, err)
		}
		if re, _ := json.Marshal(&back); string(re) != string(want) {
			t.Errorf("round-trip %s -> %s", want, re)
		}
	}
}
