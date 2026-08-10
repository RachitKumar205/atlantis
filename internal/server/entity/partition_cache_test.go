package entity

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func cacheIR(partitionField string) (*dsl.Entity, *dsl.IR) {
	e := dsl.Entity{
		Name: "Doc", Namespace: "pc", PartitionField: partitionField,
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}
	ir := &dsl.IR{Entities: []dsl.Entity{e}}
	return &ir.Entities[0], ir
}

// A partitioned entity must never be served from the read cache.
//
// The cache sits ABOVE SQL. A hit never reaches the database, so the row-level
// security policy never runs — and the cache key is entity plus primary key
// with no tenant in it. One tenant's cached row would be handed to another
// asking for the same id, and the policy cannot prevent it because the policy
// is never consulted. Every other guarantee in `partition by` is enforced by
// PostgreSQL; this is the one path that goes around PostgreSQL.
//
// Putting the tenant in the key does not fix it yet: the asynchronous
// invalidation worker drains an outbox with no request context, so it cannot
// build a tenant-scoped key. Its invalidations would miss and the entries
// would go stale permanently, which is worse than not caching.
func TestPartitionedEntitiesAreNeverCached(t *testing.T) {
	e, ir := cacheIR("tenant")
	meta := buildEntityMeta(e, ir, nil, nil)
	if meta.cacheable {
		t.Error("a partitioned entity is marked cacheable. The read cache is " +
			"consulted before the database, so a cached row body would be served " +
			"across tenants without row-level security ever running")
	}
}

// And the restriction is specific to partitioning — an ordinary entity keeps
// its cache, or this "fix" is just a performance regression wearing a security
// argument.
func TestUnpartitionedEntitiesKeepTheirCache(t *testing.T) {
	e, ir := cacheIR("")
	meta := buildEntityMeta(e, ir, nil, nil)
	if !meta.cacheable {
		t.Error("an entity with no `partition by` lost its read cache")
	}
}
