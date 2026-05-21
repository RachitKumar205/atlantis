# Caching and invalidation

Atlantis has two caches: a body cache keyed by primary key, and a query-result cache keyed by filter arguments. Both are per-entity opt-in via a `cache { ... }` block. Reads check memcached; writes commit to Postgres and invalidate through a transactional outbox.

## Enabling the cache

Add a `cache { ... }` block to an entity declaration:

```
entity Note in app {
  id    bigint primary serial
  title varchar(200) not null

  cache { read_through ttl=5m }
}
```

After `tide apply`, `GetNote` and `QueryNote` are served read-through against memcached with the declared TTL. `Create`, `Update`, and `Delete` go to Postgres and invalidate the cached entries (mechanism below).

## How invalidation reaches the cache

Atlantis appends one row per write to an `outbox` table inside the write's transaction. The transaction commits the data change and the enqueue together, or neither. An outbox worker drains the queue (default every 250ms; see `OUTBOX_DRAIN_INTERVAL`) and applies invalidations to memcached.

The enqueue is transactional with the data change; the cache mutation is not. Between commit and worker pickup (bounded by `OUTBOX_DRAIN_INTERVAL`, typically tens of milliseconds) a read may still hit the pre-write cached value.

A `Get` after a `Create`/`Update`/`Delete` from the same caller observes the new value: the body cache uses a version-pointer indirection updated by the write transaction itself, so it bypasses the worker lag. `Query` does not have this property — see [Bypassing the query-result cache](#bypassing-the-query-result-cache).

## The two caches

The body cache holds individual rows keyed by primary key. `Get` and entity-include lookups read it. A write to row 42 invalidates only the body entry for row 42.

The query-result cache holds `Query` result sets keyed by the filter arguments. Invalidation is per-entity rather than per-predicate: each write bumps a generation counter that is part of every cached query key, so reads after the write form keys that miss the cache and fall through to Postgres.

Per-predicate invalidation would require evaluating every cached query's filter on every write; the counter-bump model trades that work for a lower hit rate on the query-result cache after bursts of writes.

Writes are never cached. Includes resolve through the body cache by primary key.

## Bypassing the query-result cache

The body cache invalidates by primary key on commit via the version pointer, so `Get` reads its own writes without a flag. `Query` results are invalidated by counter bump on outbox-worker pickup, so a writer that must observe its own write on `Query` sets `cache_skip=true` on the request. With the flag, the server skips the query-result cache and reads from Postgres; the body cache continues to serve.

## Cache tags

A `cache { ... }` block can include a `tag` template that groups entries under a shared invalidation key:

```
entity Cart in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id

  cache { read_through ttl=5m tag="customer:{customer_id}" }
}

entity Address in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id

  cache { read_through ttl=5m tag="customer:{customer_id}" }
}
```

Both entities resolve the same tag for a given customer. A write to either entity invalidates the entire customer-scoped set across both. Reach for tags when a logical resource maps to several entities that should expire together.

## Related

- [Schema as code](schema-as-code.md) — why the cache opt-in is declared in the schema.
- [Architecture: the cache](../architecture/cache-architecture.md) — outbox worker, generation counters, and version-pointer mechanics.
- [The DSL grammar](../reference/dsl-grammar.md) — the `cache { ... }` block syntax.
