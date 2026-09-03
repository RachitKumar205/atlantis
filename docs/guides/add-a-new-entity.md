# Add a new entity

Declare a new entity in `.atl`, apply it, and regenerate the typed client.

## Prerequisites

- A service repo with `tide.yaml` ([Getting started](../getting-started/) creates one).
- `tide` on `$PATH`, enrolled via `tide login`.

## 1. Create the schema file

Under a path listed in `tide.yaml`'s `schema_paths`:

```
internal/orders/schema.atl
```

## 2. Declare the entity

```atl
entity Order in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id
  total       numeric(10, 2) not null
  created_at  timestamptz not null default now()
}
```

## 3. Plan and apply

```bash
tide plan
tide apply
```

`tide plan` reports what would change without mutating. `tide apply` runs the migration and updates the merged schema.

To preview behaviour before applying — seed rows, run queries, capture and diff state — boot a copy of the schema in the [sandbox](use-the-sandbox.md).

Common errors:

- `references unknown entity shop.Customer`: the referenced entity hasn't been registered yet. Run `tide apply` from the repo that declares `shop.Customer` first.
- `tide plan` exits 1 (backfill required): you've added a `not null` column without a `default` to an existing entity, or added a composite `unique by a, b` to an existing entity (it can fail on existing duplicate tuples — verify none exist). Add a `default` / dedupe, or supply backfill SQL.
- `tide plan` exits 2 (cross-caller breaking): another caller's schema depends on a field you removed or renamed. The output names the conflict.

## 4. Regenerate the client

```bash
tide generate
```

`tide generate` rewrites the client under `output_dir` from the server's
canonical schema, scoped to the namespaces in `generate:`. Commit the
result with the `.atl` change. An old client keeps working without it;
code referring to the new entity does not compile until you regenerate.

## 5. Verify

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

`touch_on_update by` installs a Postgres trigger that sets `updated_at` on every `UPDATE`.

### Soft delete

```atl
deleted_at timestamptz
soft_delete by deleted_at
```

The generated `Delete` RPC sets `deleted_at` to `now()` instead of dropping the row. `Get`, `List`, and `Query` filter `deleted_at IS NULL` automatically. To read tombstones, declare a [custom query](../concepts/custom-queries-and-procedures.md) with the inverse filter.

### Per-tenant partition

When one table holds rows for many tenants, name the column that says which
tenant a row belongs to:

```atl
tenant_id varchar(8) not null
partition by tenant_id
```

The column must be `not null`. A NULL in that column matches no tenant's
policy, so the row becomes invisible to everyone — including whoever wrote it.

**Enforcement is PostgreSQL's.** Applying this emits `ENABLE` and
`FORCE ROW LEVEL SECURITY` on the table plus a policy comparing the column
to the tenant bound for the current transaction, in both `USING` (what a
statement may read) and `WITH CHECK` (what it may write). On a `uuid` or
`bigint` column the comparison casts the tenant value, not the column, so
the column's index stays usable. Every path is covered, including custom
query bodies.

An index on the column is emitted too — always, even if you already
declare one covering it, because the policy predicate runs on every read:
on 200k rows, a scan without the index measured 590 ms against 0.046 ms
with it. If you declared your own covering index, you will have two;
remove yours if you want one.

### Two policies

`partition by` emits `<table>_tenant_isolation`, a `RESTRICTIVE` policy
comparing the column to `atlantis.current_partition()` — the tenant
boundary, ANDed with every other policy. A table also needs at least one
`PERMISSIVE` policy to return rows at all; when none exists, the apply
emits `<table>_default_access`, `PERMISSIVE USING (true)`, so within a
tenant every statement proceeds.

Narrower per-row access control — this user sees only their own invoices —
is not declarable through the DSL yet; the tenant boundary is the
isolation the platform enforces.

**Adding it later produces a migration.** `partition by` is diffed, so you can
add it to an entity that already exists, move it to another column, or remove
it. The plan is classified cross-caller breaking: enabling isolation means a
request that carries no tenant reads nothing from that table, and removing it
means every caller reads every tenant's rows. Neither is applied without an
explicit decision.

**What is trusted.** Your service asserts which tenant a request is for —
the `atlantis-tenant` request header — and atlantis does not derive or
second-guess that. Once asserted, every statement in the transaction is
confined to that tenant. The guarantee stops the accidental leak; the
tenant assertion itself is trusted, so a caller that names the wrong
tenant is not caught.

**How the tenant reaches the policy.** The tenant is a transaction-local
run-time parameter, `atlantis.tenant`, compared through
`atlantis.current_partition()`. Transaction-local means it reverts when the
transaction ends, so a value cannot outlive the request that set it on a
pooled connection. Because PostgreSQL cannot lock a custom parameter, two
protections close the rebinding route:

- `tide apply` rejects `set_config` and `atlantis.set_partition` in every
  piece of SQL you write: query bodies, procedure steps, `check`
  expressions, and partial-index predicates. A bare `SET` is rejected too.
- Each pooled connection is opened with the parameter cleared, so a value
  cannot arrive from a role default or a connection pooler.

Every entity RPC, custom query, and custom procedure binds the tenant
before touching the database, and the platform runs the managed database on
a role that cannot bypass row-level security — a bypassing role would leave
the policy attached and inert.

### What does not work on a partitioned entity yet

| Feature | Behaviour with `partition by` |
|---|---|
| **`ttl_field`** | `tide apply` refuses the combination unless the entity is a hypertable on its TTL column — see [row TTL](row-ttl.md). |
| **Backfills** | `tide apply --backfill` refuses a partitioned entity by name. A chunked backfill has to cover every tenant, and there is no single tenant to bind. |
| **The sandbox** | The in-memory backend does not enforce the policies. Do not use it to test isolation. |

## Related

- [DSL grammar reference](../reference/dsl-grammar.md) — every modifier and entity-level clause.
- [Type mapping](../reference/dsl-types.md) — each `.atl` type's Postgres, Go, and proto representation.
