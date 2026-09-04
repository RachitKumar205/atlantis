# Add a new entity

Declare a new entity in `.atl`, apply it, and regenerate the typed client.

## Prerequisites

- A caller set up with `tide` ([Get started](../getting-started/index.md)),
  and a repository with `tide.yaml`.

## 1. Declare the entity

Create a file under a path listed in `tide.yaml`'s `schema_paths`:

```text
internal/orders/schema.atl
```

```atl
entity Order in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id
  total       numeric(10, 2) not null
  created_at  timestamptz not null default now()
}
```

## 2. Plan and apply

```bash
tide plan
tide apply
```

`tide plan` reports what would change without mutating. `tide apply` runs
the migration and updates the merged schema.

## 3. Regenerate the client

```bash
tide generate
```

`tide generate` rewrites the client under `output_dir` from the server's
canonical schema, scoped to the namespaces in `generate:`. Commit the
result with the `.atl` change. An old client keeps working without it;
code referring to the new entity does not compile until you regenerate.

## Verify

```bash
tide show internal/orders
```

Prints each submitted file whose path contains the argument — here, the
file with the new entity as the server now holds it:

```
--- internal/orders/schema.atl ---
entity Order in shop {
  ...
}
```

## Common patterns

### Server-set timestamps with auto-touch

```atl
created_at timestamptz not null default now()
updated_at timestamptz not null default now()
touch_on_update by updated_at
```

`touch_on_update by` installs a Postgres trigger that sets `updated_at`
on every `UPDATE`.

### Soft delete

```atl
deleted_at timestamptz
soft_delete by deleted_at
```

The generated `Delete` RPC sets `deleted_at` to `now()` instead of
dropping the row. `Get` and `Query` filter `deleted_at IS NULL`
automatically. To read tombstones, declare a
[custom query](../concepts/custom-queries-and-procedures.md) with the
inverse filter.

### Per-tenant partition

When one table holds rows for many tenants, name the column that says
which tenant a row belongs to:

```atl
tenant_id varchar(8) not null
partition by tenant_id
```

The column must be `not null`. A NULL in that column matches no tenant's
policy, so the row becomes invisible to everyone, including whoever wrote
it. [Tenant isolation](../concepts/tenant-isolation.md) covers what
PostgreSQL enforces, what atlantis trusts, and what does not work on a
partitioned entity.

## Common errors

- `references unknown entity shop.Customer` — the referenced entity is
  not registered yet. Run `tide apply` from the repo that declares
  `shop.Customer` first.
- `tide plan` exits 1 (backfill required) — you added a `not null` column
  without a `default` to an existing entity, or added a composite
  `unique by a, b` to an existing entity, which can fail on existing
  duplicate tuples. Add a `default`, dedupe, or supply backfill SQL.
- `tide plan` exits 2 (cross-caller breaking) — another caller's schema
  depends on a field you removed or renamed. The output names the
  conflict.

## Related

- [Tenant isolation](../concepts/tenant-isolation.md) — what `partition by`
  enforces and what it trusts.
- [Use the sandbox](use-the-sandbox.md) — boot a copy of the schema to
  seed rows, run queries, and diff state before applying.
- [DSL grammar](../reference/dsl-grammar.md) — every modifier and
  entity-level clause.
- [DSL types](../reference/dsl-types.md) — each `.atl` type's Postgres,
  Go, and proto representation.
