# Ephemeral data

An `ephemeral` declaration is a typed data shape backed by memcached, not
Postgres. No table, no migration, no VACUUM. The data lives in the cache
layer with a declared TTL.

```atl
ephemeral EphemeralOutfit in consumer {
  key           varchar(255) primary
  variant_ids   []text not null
  product_ids   []text not null
  user_item_ids []text
  ttl = 24h
}
```

`tide generate` writes a typed store with three methods:

```go
store := &consumer.EphemeralOutfitStore{MC: memcachedClient}

store.Set(ctx, "outfit:abc123", consumer.EphemeralOutfitValue{
    VariantIds: []string{"v1", "v2"},
    ProductIds: []string{"p1", "p2"},
})

val, err := store.Get(ctx, "outfit:abc123")

store.Delete(ctx, "outfit:abc123")
```

## Choosing between ephemeral and entity

| | `entity` | `ephemeral` |
|---|---|---|
| Storage | Postgres row | Memcached entry |
| Durability | Survives restarts, replicated | Lost on cache eviction or restart |
| TTL | `ttl_field` + SweepExpired job (Postgres DELETE) | Native memcached TTL (zero I/O on expiry) |
| Queryability | Full SQL (indexes, FKs, JOINs) | Key-lookup only |
| Use case | Durable state, relationships, audit | Short-lived scratch data the caller can regenerate |

Two considerations the table does not hold. High write-and-expire volume
bloats a Postgres table with dead tuples, which is the case `ephemeral`
avoids. And `ephemeral` suits data with a natural expiry measured in
hours, not days.

## How it works

`Set` serialises the value struct as JSON and writes it to memcached with
the declared TTL as the expiry. `Get` reads the raw bytes and
deserialises them into the typed struct; a cache miss returns an error,
and the caller regenerates or falls back to a durable source. `Delete` is
a memcached DELETE, optional because the TTL handles cleanup.

The generated code prefixes every key with `eph:<namespace>.<Name>:`, so
ephemeral entries collide neither with entity cache entries nor with each
other. The memcached client is the one atlantis already runs for entity
read-through caching.

## Limits

- **Value size**: memcached's default slab limit is 1 MB per key, and the
  JSON-serialised value must fit. For large blobs, use an entity with a
  `bytea` column.
- **Eviction pressure**: memcached evicts LRU entries when memory is
  full, even before the TTL expires. Ephemeral data is a performance
  optimisation, not a durability guarantee; callers handle a miss by
  regenerating or falling back to a durable source.

## Related

- [Caching and invalidation](caching-and-invalidation.md) — the entity-level cache layer ephemeral builds on.
- [Expire rows automatically](../guides/row-ttl.md) — the Postgres-durable alternative, and the SweepExpired job behind it.
- [Jobs and workflows](jobs-and-workflows.md) — the runtime that SweepExpired runs on.
