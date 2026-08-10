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

// The policy predicate runs on every read, so it needs an index — and the
// index must be on the discriminator, which is the part a name check cannot
// see. An earlier version of this test asserted only that something called
// "_partition_idx" appeared; emitting it on the wrong column passed the entire
// package, because an index on any column is valid DDL.
func TestPartitionIndexIsOnTheDiscriminator(t *testing.T) {
	out, err := EmitInitial(partIR("tenant", dsl.FieldType{Name: "text"}, nil))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	line := indexLine(out.Up, "_partition_idx")
	if line == "" {
		t.Fatalf("no index backing the policy predicate. Without one every read "+
			"of this table degrades roughly a thousandfold:\n%s", out.Up)
	}
	if !strings.Contains(line, `("tenant")`) {
		t.Errorf("the policy's index is not on the partition column, so it cannot "+
			"serve the predicate it exists for:\n%s", line)
	}
}

// It is emitted unconditionally, including when the schema already declares
// something covering the column.
//
// Skipping in that case is what the previous version did, and it tied a
// safety-critical index to a declaration somebody may reasonably delete:
// emitPartitionPolicy runs only at entity creation, so dropping the `unique by`
// took the index with it, via a migration classified ADDITIVE. A duplicate
// btree is bounded and visible; a silently missing one is not.
func TestPartitionIndexIsEmittedEvenWhenTheSchemaCoversTheColumn(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(e *dsl.Entity)
	}{
		{"declared btree on the column", func(e *dsl.Entity) {
			e.Indexes = []dsl.Index{{Kind: dsl.IndexBtree,
				Fields: []dsl.IndexField{{Name: "tenant"}}}}
		}},
		{"unique by leading with it", func(e *dsl.Entity) {
			e.Uniques = []dsl.UniqueSpec{{Fields: []string{"tenant", "body"}}}
		}},
		{"partial index leading with it", func(e *dsl.Entity) {
			e.Indexes = []dsl.Index{{Kind: dsl.IndexPartial,
				Fields: []dsl.IndexField{{Name: "tenant"}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ir := partIR("tenant", dsl.FieldType{Name: "text"}, nil)
			tc.mut(&ir.Entities[0])
			out, err := EmitInitial(ir)
			if err != nil {
				t.Fatalf("EmitInitial: %v", err)
			}
			if indexLine(out.Up, "_partition_idx") == "" {
				t.Errorf("skipped the policy's own index because the schema declares "+
					"%s. That construct is droppable and this index is never "+
					"re-emitted, so removing it silently removes tenant isolation's "+
					"index through an additive migration:\n%s", tc.name, out.Up)
			}
		})
	}
}

func indexLine(sql, needle string) string {
	for _, l := range strings.Split(sql, "\n") {
		if strings.Contains(l, "CREATE INDEX") && strings.Contains(l, needle) {
			return l
		}
	}
	return ""
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
		{Name: "citext"},
		{Name: "uuid"},
		{Name: "bigint"},
		{Name: "integer"},
		{Name: "smallint"},
		{Name: "boolean"},
		{Name: "timestamptz"},
		{Name: "date"},
		{Name: "numeric", NumP: 10, NumS: 2, HasNumP: true},
		{Name: "bytea"},
		{Name: "jsonb"},
		// Arrays are the shape that broke: "varchar(20)[]" matched the varchar
		// prefix, skipped the cast, and emitted a policy PostgreSQL refuses.
		{Name: "text", Array: true, Elem: &dsl.FieldType{Name: "text"}},
		{Name: "varchar", Len: 20, Array: true,
			Elem: &dsl.FieldType{Name: "varchar", Len: 20}},
	} {
		name := ft.Name
		if ft.Array {
			name += "_array"
		}
		t.Run(name, func(t *testing.T) {
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
			// Twice. The initial script promises to be re-appliable after a
			// partial failure, and CREATE POLICY has no IF NOT EXISTS — so
			// without the DROP POLICY IF EXISTS ahead of it, the retry after a
			// half-applied migration fails on the one statement that carries
			// the tenant isolation.
			if _, err := conn.Exec(ctx, out.Up); err != nil {
				t.Fatalf("the emitted DDL for a %s discriminator is not "+
					"re-appliable, so a retry after a partial apply cannot "+
					"succeed: %v\n%s", ft.Name, err, out.Up)
			}
		})
	}
}

// The same property without a database, because CI runs no Postgres and the
// test above therefore skips there.
//
// Ordering, not mere presence: a DROP emitted after the CREATE would satisfy a
// substring check and drop the policy it just created, leaving the table with
// RLS enabled, no policy, and every read returning nothing.
func TestPartitionPolicyDDLIsReappliable(t *testing.T) {
	out, err := EmitInitial(partIR("tenant", dsl.FieldType{Name: "text"}, nil))
	if err != nil {
		t.Fatalf("EmitInitial: %v", err)
	}
	drop := strings.Index(out.Up, "DROP POLICY IF EXISTS")
	create := strings.Index(out.Up, "CREATE POLICY")
	switch {
	case create < 0:
		t.Fatal("no policy emitted for a partitioned entity")
	case drop < 0:
		t.Error("CREATE POLICY is emitted with no DROP POLICY IF EXISTS before " +
			"it. CREATE POLICY has no IF NOT EXISTS, so re-applying the initial " +
			"script after a partial failure fails on the statement that carries " +
			"tenant isolation, and the operator is left hand-editing DDL nobody wrote")
	case drop > create:
		t.Errorf("DROP POLICY IF EXISTS is emitted AFTER CREATE POLICY, so applying "+
			"the script drops the policy it just created: RLS stays enabled with "+
			"no policy, and every read returns nothing\n%s", out.Up)
	}
}
