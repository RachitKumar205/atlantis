package sandbox

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// `real` and `double` must map to a float kind, not to opaque bytes.
//
// colKindFor ends in a deliberate fallback: an unrecognised PG type becomes
// KindBytes so a caller's custom domain or a type the sandbox has not learned
// yet cannot stop it booting. That fallback is right for an UNKNOWN type and
// wrong for a documented one — `real` and `double` took it, so a float column
// round-tripped as bytes and every comparison against it failed in the
// executor with "opaque column".
//
// catalog.go states the invariant this broke: "The set is closed: every
// supported PG-side type maps onto exactly one ColKind ... so the fidelity
// matrix stays honest." A supported type landing on the unknown-type fallback
// is that matrix quietly lying.
func TestFloatColumnsGetAFloatKind(t *testing.T) {
	for _, tc := range []struct {
		atl  string
		want sim.ColKind
	}{
		{"real", sim.KindFloat32},
		{"double", sim.KindFloat64},
	} {
		t.Run(tc.atl, func(t *testing.T) {
			got, err := colKindFor(dsl.FieldType{Name: tc.atl})
			if err != nil {
				t.Fatalf("colKindFor(%s): %v", tc.atl, err)
			}
			if got == sim.KindBytes {
				t.Fatalf("`%s` maps to KindBytes — the fallback for types the "+
					"sandbox does not recognise. It is a documented type, so it "+
					"round-trips as opaque bytes and any WHERE against it fails "+
					"in the executor", tc.atl)
			}
			if got != tc.want {
				t.Errorf("colKindFor(%s) = %v, want %v. The width matters: "+
					"comparing a float4 value at float8 selects different rows "+
					"than Postgres would", tc.atl, got, tc.want)
			}
		})
	}
}

// A float column filters and orders the way Postgres does.
//
// The kind mapping alone proves nothing — the executor has to act on it. This
// runs the comparisons a caller's generated query would, through the same
// simulator a sandbox session uses.
func TestSandboxFiltersAndOrdersFloats(t *testing.T) {
	cat := sim.NewCatalog()
	if err := cat.RegisterTable(&sim.TableDesc{
		Schema: "atlantis",
		Name:   "library_book",
		Cols: []sim.Column{
			{Name: "id", Kind: sim.KindInt64},
			{Name: "score", Kind: sim.KindFloat32, Nullable: true},
			{Name: "weight", Kind: sim.KindFloat64, Nullable: true},
		},
		PKCols: []string{"id"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	pool := sim.NewPool(cat)
	ctx := t.Context()

	const ins = `INSERT INTO "atlantis"."library_book" ("id", "score", "weight") ` +
		`VALUES ($1, $2, $3) RETURNING "id"`
	for _, r := range []struct {
		id     int64
		score  any
		weight any
	}{
		{1, float32(0.5), 0.5},
		{2, float32(2.5), 2.5},
		{3, float32(10.25), 10.25},
		{4, nil, nil},
	} {
		if err := pool.QueryRow(ctx, ins, r.id, r.score, r.weight).Scan(new(int64)); err != nil {
			t.Fatalf("seed %d: %v", r.id, err)
		}
	}

	collect := func(t *testing.T, sqlText string, args ...any) []int64 {
		t.Helper()
		rows, err := pool.Query(ctx, sqlText, args...)
		if err != nil {
			t.Fatalf("query: %v\nSQL: %s", err, sqlText)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if serr := rows.Scan(&id); serr != nil {
				t.Fatalf("scan: %v", serr)
			}
			ids = append(ids, id)
		}
		if rerr := rows.Err(); rerr != nil {
			t.Fatalf("rows: %v", rerr)
		}
		return ids
	}

	t.Run("filter on a real column", func(t *testing.T) {
		got := collect(t, `SELECT "id" FROM "atlantis"."library_book" `+
			`WHERE "score" > $1 ORDER BY "id" ASC`, float32(2.5))
		if len(got) != 1 || got[0] != 3 {
			t.Errorf("score > 2.5 returned %v, want [3]", got)
		}
	})

	t.Run("filter on a double column", func(t *testing.T) {
		got := collect(t, `SELECT "id" FROM "atlantis"."library_book" `+
			`WHERE "weight" <= $1 ORDER BY "id" ASC`, 2.5)
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Errorf("weight <= 2.5 returned %v, want [1 2]", got)
		}
	})

	t.Run("order by a real column puts NULLs last ascending", func(t *testing.T) {
		got := collect(t, `SELECT "id" FROM "atlantis"."library_book" `+
			`ORDER BY "score" ASC NULLS LAST`)
		want := []int64{1, 2, 3, 4}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("IS NULL finds the null float", func(t *testing.T) {
		got := collect(t, `SELECT "id" FROM "atlantis"."library_book" `+
			`WHERE "score" IS NULL`)
		if len(got) != 1 || got[0] != 4 {
			t.Errorf("score IS NULL returned %v, want [4]", got)
		}
	})
}
