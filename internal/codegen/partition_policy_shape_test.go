package codegen

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func partIR(field string, ft dsl.FieldType, idx []dsl.Index) *dsl.IR {
	return &dsl.IR{Entities: []dsl.Entity{{
		Name: "Doc", Namespace: "part", PartitionField: field,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: field, Type: ft, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
		Indexes: idx,
	}}}
}

// The discriminator is text. Casting the COLUMN to match it destroys the index
// and turns the policy into a per-row function call; casting the FUNCTION
// keeps an index condition and one call per query. Measured on PG 17.8 over
// 200k rows: 0.29 ms against 286 ms.
//
// And with no cast at all, a non-text column cannot have the policy created —
// `operator does not exist: uuid = text` — which surfaces at apply time
// against DDL nobody hand-wrote.
func TestPartitionPolicyCastsTheFunctionNotTheColumn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ft       dsl.FieldType
		wantCast string
	}{
		{"text needs no cast", dsl.FieldType{Name: "text"}, ""},
		{"varchar needs no cast", dsl.FieldType{Name: "varchar", Len: 32}, ""},
		{"uuid casts the function", dsl.FieldType{Name: "uuid"}, "::uuid"},
		{"bigint casts the function", dsl.FieldType{Name: "bigint"}, "::bigint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := EmitInitial(partIR("tenant", tc.ft, nil))
			if err != nil {
				t.Fatalf("EmitInitial: %v", err)
			}
			sql := out.Up

			if !strings.Contains(sql, "CREATE POLICY") {
				t.Fatal("no policy emitted for a partitioned entity")
			}
			// The column must never be cast — that is the slow shape.
			if strings.Contains(sql, `"tenant"::`) {
				t.Errorf("the policy casts the COLUMN, which drops the index condition "+
					"and re-invokes current_partition() once per row scanned:\n%s",
					policyLine(sql))
			}
			if tc.wantCast == "" {
				if strings.Contains(sql, "current_partition()::") {
					t.Errorf("cast a text-shaped column unnecessarily:\n%s", policyLine(sql))
				}
			} else if !strings.Contains(sql, "current_partition()"+tc.wantCast) {
				t.Errorf("no %s cast on the discriminator; the policy cannot be created "+
					"against this column type at all:\n%s", tc.wantCast, policyLine(sql))
			}
		})
	}
}

// The policy predicate runs on every read. Whether it lands in an Index Cond or
// a Filter is the difference between one function call per query and one per
// row scanned.
func TestPartitionFieldGetsAnIndexUnlessOneExists(t *testing.T) {
	t.Run("emitted when nothing covers it", func(t *testing.T) {
		out, err := EmitInitial(partIR("tenant", dsl.FieldType{Name: "text"}, nil))
		if err != nil {
			t.Fatalf("EmitInitial: %v", err)
		}
		if !strings.Contains(out.Up, `("tenant")`) || !strings.Contains(out.Up, "CREATE INDEX") {
			t.Errorf("no index on the partition column. Without one the policy "+
				"predicate becomes a per-row filter and every read of this table "+
				"degrades by roughly a thousandfold:\n%s", out.Up)
		}
	})

	t.Run("not duplicated when the schema leads an index with it", func(t *testing.T) {
		idx := []dsl.Index{{Kind: dsl.IndexBtree, Fields: []dsl.IndexField{{Name: "tenant"}}}}
		out, err := EmitInitial(partIR("tenant", dsl.FieldType{Name: "text"}, idx))
		if err != nil {
			t.Fatalf("EmitInitial: %v", err)
		}
		if strings.Count(out.Up, `("tenant")`) > 1 {
			t.Errorf("emitted a second index on a column the schema already indexes:\n%s",
				out.Up)
		}
	})
}

func policyLine(sql string) string {
	for _, l := range strings.Split(sql, "\n") {
		if strings.Contains(l, "CREATE POLICY") {
			return l
		}
	}
	return "(no CREATE POLICY line)"
}

// And the emitted DDL must actually apply — the failure this guards against is
// a policy PostgreSQL refuses to create, which no amount of string assertion
// would catch.
func TestPartitionPolicyAppliesForEveryColumnType(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to execute the emitted policy DDL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	for _, ft := range []dsl.FieldType{
		{Name: "text"},
		{Name: "varchar", Len: 32},
		{Name: "uuid"},
		{Name: "bigint"},
	} {
		t.Run(ft.Name, func(t *testing.T) {
			_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.part_doc CASCADE`)
			t.Cleanup(func() {
				_, _ = conn.Exec(ctx, `DROP TABLE IF EXISTS atlantis.part_doc CASCADE`)
			})

			out, err := EmitInitial(partIR("tenant", ft, nil))
			if err != nil {
				t.Fatalf("EmitInitial: %v", err)
			}
			if _, err := conn.Exec(ctx, out.Up); err != nil {
				t.Fatalf("the emitted policy DDL for a %s discriminator does not "+
					"apply: %v\n%s", ft.Name, err, out.Up)
			}
		})
	}
}
