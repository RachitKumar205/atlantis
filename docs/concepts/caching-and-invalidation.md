# Caching and invalidation

atlantis has two caches: a body cache keyed by primary key, and a query-result cache keyed by filter arguments. Both are per-entity opt-in via a `cache { ... }` block. Reads check memcached; writes commit to Postgres and invalidate through a transactional outbox.

## Enabling the cache

Add a `cache { ... }` block to an entity declaration:

```atl
entity Note in app {
  id    bigint primary serial
  title varchar(200) not null

  cache { read_through ttl=5m }
}
```

After `tide apply`, `GetNote` and `QueryNote` are served read-through against memcached with the declared TTL. `Create`, `Update`, and `Delete` go to Postgres and invalidate the cached entries (mechanism below).

## How invalidation reaches the cache

atlantis appends one row per write to an outbox table inside the write's transaction. The transaction commits the data change and the enqueue together, or neither. An outbox worker drains the queue about every 250 milliseconds and applies invalidations to memcached.

The enqueue is transactional with the data change; the cache mutation is not. Between commit and worker pickup a read may still hit the pre-write cached value. The window is bounded by the drain cadence, 250 milliseconds by default, and it applies to `Get` and `Query` alike.

## The two caches

The body cache holds individual rows keyed by primary key. `Get` and entity-include lookups read it. A write to row 42 invalidates only the body entry for row 42.

The query-result cache holds `Query` result sets keyed by the filter arguments. Invalidation is per-entity: when the worker picks up a write, it bumps a generation counter that is part of every cached query key, so later reads form keys that miss the cache and fall through to Postgres. After a burst of writes to an entity, its query-result hit rate drops until reads repopulate the cache.

Writes are never cached. Entity includes resolve through the body cache by primary key.

## Bypassing the query-result cache

To observe your own write on `Query`, set `cache_skip=true` on the request. With the flag, the server skips the query-result cache and reads from Postgres; the body cache continues to serve. `Get` has no such flag, so a `Get` inside the invalidation window can return the pre-write row.

## What can go stale, and for how long

- A read inside the invalidation window returns a stale but self-consistent
  row: a complete earlier version, never a mix of old and new fields.
- The window normally closes within a few hundred milliseconds of the
  commit. A failed invalidation retries, so under fault the window
  stretches and the invalidation is not lost.
- Pending invalidations live in Postgres, in the same transaction as the
  write, so a crash between commit and cache update delays the invalidation
  and does not lose it.
- A cache restart is a cold cache: reads miss to Postgres and repopulate.
  Latency rises until the working set rewarms; no read returns wrong data.

## Cache tags

A `cache { ... }` block can include a `tag` template that groups entries under a shared invalidation key:

```atl
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
- [The apply path](how-atlantis-runs-your-schema.md) — where the cache sits in the serving path.
- [The DSL grammar](../reference/dsl-grammar.md) — the `cache { ... }` block syntax.
