package introspect

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// countingDB counts what reaches the wire. Begin is counted rather than the
// statements inside the transaction, because a transaction opened per entity
// is the shape that made this slow.
type countingDB struct {
	inner   DBTX
	queries atomic.Int64
	begins  atomic.Int64
}

func (c *countingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.queries.Add(1)
	return c.inner.Query(ctx, sql, args...)
}

func (c *countingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	c.begins.Add(1)
	return c.inner.Begin(ctx)
}

// Column-type drift costs the same number of round trips whatever the schema
// size.
//
// It rendered each entity's declared types onto its own TEMP table, in its own
// transaction: Begin, CREATE, SELECT, Rollback, once per entity. Against a
// database across the internet that is the whole cost — 97 entities at 165ms
// to EBI's public mirror is 64 seconds before any comparison happens, and the
// console sat on "Planning…" for it.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@host:5432/atlantis \
//	    go test ./internal/introspect -run RoundTrips -count=1
func TestColumnTypeDriftRoundTripsDoNotScaleWithSchemaSize(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to count round trips against a real database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	build := func(n int) *dsl.IR {
		ir := &dsl.IR{}
		for i := 0; i < n; i++ {
			ir.Entities = append(ir.Entities, dsl.Entity{
				Name:      "E" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
				Namespace: "rtcount",
				Fields: []dsl.Field{
					{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
					{Name: "label", Type: dsl.FieldType{Name: "varchar", Len: 40}},
					{Name: "at", Type: dsl.FieldType{Name: "timestamptz"}},
				},
			})
		}
		return ir
	}

	measure := func(n int) (queries, begins int64) {
		c := &countingDB{inner: pool}
		if _, _, err := DetectColumnTypeDrift(ctx, c, build(n)); err != nil {
			t.Fatalf("DetectColumnTypeDrift(%d): %v", n, err)
		}
		return c.queries.Load(), c.begins.Load()
	}

	q1, b1 := measure(2)
	q2, b2 := measure(40)

	t.Logf("2 entities: %d queries, %d transactions", q1, b1)
	t.Logf("40 entities: %d queries, %d transactions", q2, b2)

	if b2 > b1 {
		t.Errorf("transactions grew from %d to %d as the schema grew from 2 to 40 "+
			"entities; the cost is one round trip per entity across the internet", b1, b2)
	}
	if q2 > q1 {
		t.Errorf("queries grew from %d to %d as the schema grew from 2 to 40 entities", q1, q2)
	}
}
