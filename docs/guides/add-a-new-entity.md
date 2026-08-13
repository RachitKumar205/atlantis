# Add a new entity

After this recipe you'll have a new entity declared, validated, applied, and registered in the merged schema.

Prereqs:

- A service repo with `tide.yaml` (see [Getting started](../getting-started/) if you haven't created one).
- `tide` on `$PATH`.
- The Atlantis server reachable on `tide.yaml`'s `endpoint`.

## 1. Create the schema file

Under a path listed in `tide.yaml`'s `schema_paths`:

```
internal/orders/schema.atl
```

## 2. Declare the entity

```
entity Order in shop {
  id          bigint primary serial
  customer_id varchar(8) not null references shop.Customer.id
  total       numeric(10, 2) not null
  created_at  timestamptz not null default now()
}
```

## 3. Plan and apply

```
tide plan
tide apply
```

`tide plan` reports what would change without mutating. `tide apply` runs the migration and updates the merged schema.

To preview behaviour before applying — seed rows, run queries, capture and diff state — boot a copy of the schema in the [sandbox](use-the-sandbox.md).

Common errors:

- `references unknown entity shop.Customer`: the referenced entity hasn't been registered yet. Run `tide apply` from the repo that declares `shop.Customer` first.
- `tide plan` exits 1 (backfill required): you've added a `not null` column without a `default` to an existing entity, or added a composite `unique by a, b` to an existing entity (it can fail on existing duplicate tuples — verify none exist). Add a `default` / dedupe, or supply backfill SQL.
- `tide plan` exits 2 (cross-caller breaking): another caller's schema depends on a field you removed or renamed. The output names the conflict.

## 4. Verify

```
tide show Order
```

Prints the canonical `.atl` text as the server now holds it.

## Common patterns

### Server-set timestamps with auto-touch

```
created_at timestamptz not null default now()
updated_at timestamptz not null default now()
touch_on_update by updated_at
```

`touch_on_update by` installs a Postgres trigger that sets `updated_at` on every `UPDATE`.

### Soft delete

```
deleted_at timestamptz
soft_delete by deleted_at
```

The generated `Delete` RPC sets `deleted_at` to `now()` instead of dropping the row. `Get` and `Query` filter `deleted_at IS NULL` automatically. To read tombstones, declare a [custom query](../concepts/custom-queries-and-procedures.md) with the inverse filter.

### Per-tenant partition

When one table holds rows for many tenants, name the column that says which
tenant a row belongs to:

```
tenant_id varchar(8) not null
partition by tenant_id
```

The column must be `not null`. A NULL in that column matches no tenant's
policy, so the row becomes invisible to everyone — including whoever wrote it.

**Enforcement is PostgreSQL's, not atlantis's.** Applying this emits `ENABLE`
and `FORCE ROW LEVEL SECURITY` on the table plus a policy comparing the column
to the tenant bound for the current transaction, in both `USING` (what a
statement may read) and `WITH CHECK` (what it may write). The discriminator is
text, so on a `uuid` or `bigint` column the comparison casts the tenant value
rather than the column — casting the column would drop the index and re-run the
lookup for every row scanned.

An index on the column is emitted too — always, even if you already declare
one covering it. The policy's index belongs to the policy: anything else is
droppable, and this index is not re-emitted, so tying its lifetime to your
`unique by` would mean deleting that constraint silently removes tenant
isolation's index through a migration classified additive. If you declared your
own, you will have two; remove yours if you want one.

It matters because the policy predicate runs on every read, and whether it
lands in an index condition or a per-row filter decides how often the tenant
lookup happens. Over 200k rows on PostgreSQL 17.8 the scan is about 5x
(23.5 ms against 4.3 ms) and reads far more of the table. On a primary-key
point read the index makes no difference at all — the plan is the same either
way. The larger figures you may see quoted elsewhere were measured against the
older table-backed discriminator, which cost a table lookup per row; the
current one is a parameter read and much cheaper to get wrong.

Enforcing in the database rather than in each generated read is the point. A
predicate appended per read leaks the moment a handler is added without it, and
a custom query body is opaque text with nowhere to inject one.

**Adding it later produces a migration.** `partition by` is diffed, so you can
add it to an entity that already exists, move it to another column, or remove
it. The plan is classified cross-caller breaking: enabling isolation means a
request that carries no tenant reads nothing from that table, and removing it
means every caller reads every tenant's rows. Neither is applied without an
explicit decision.

**What is trusted.** Your service asserts which tenant a request is for, and
atlantis does not derive or second-guess that. Once asserted, every statement in
the transaction is confined to that tenant — which stops the accidental leak,
not a caller naming the wrong tenant deliberately. The guarantee is narrower
than "callers cannot lie", and more useful.

**How the tenant reaches the policy.** The tenant is a transaction-local
run-time parameter, `atlantis.tenant`, and the policy compares against it
through `atlantis.current_partition()`. Transaction-local means it reverts when
the transaction ends, so a value cannot outlive the request that set it on a
pooled connection. (Reading on: the mechanism is in place and tested; the
request-time call that sets it is not landed yet. See the runtime conditions
below.)

PostgreSQL will not lock a custom parameter — `REVOKE SET ON PARAMETER` has no
effect on one — so SQL running in the same transaction could otherwise reassign
it. Two things prevent that, and both are load-bearing:

- `tide apply` rejects `set_config` and `atlantis.set_partition` in every piece
  of SQL you write: query bodies, procedure steps, `check` expressions, and
  partial-index predicates. A bare `SET` has always been rejected.
- Each pooled connection is opened with the parameter cleared, so a value
  cannot arrive from a role default or a connection pooler.

`atlantis.set_partition` also refuses to bind twice in one transaction. That is
not a third protection and should not be relied on as one — `set_config` writes
the parameter without going through it. It catches a double bind, which is a
bug worth an error.

The residual exposure is worth stating: someone who can get a query body
through `tide apply` might find a route the validator does not know about. That
same person can drop the policy, redeclare the entity without `partition by`,
or write a body that reads whatever they like. This protects against the
accidental leak, which is the one that happens.

**Two runtime conditions.** Neither is a schema property, so neither is checked
at plan time:

- The server must connect as a role that does not bypass row-level security. A
  superuser sees through `FORCE`, and so does any role holding `BYPASSRLS` —
  the policy stays attached and completely inert. Checked at boot; set
  `ATL_REQUIRE_TENANT_ISOLATION=true` to make it refuse to start.
- Something must bind the tenant for the request. **Nothing does yet.**

Until that second condition is met, what a partitioned entity does depends
entirely on the first. On a role row-level security applies to, reads return
zero rows and **every insert fails** with `new row violates row-level security
policy` — the right direction to fail in, but the entity is unusable. **On a
role that bypasses RLS, including the superuser the default
`docker-compose.yml` connects as, reads return every tenant's rows.**

Treat `partition by` as not yet a working feature, and check which role your
deployment uses before relying on it.

## Related

- [DSL grammar reference](../reference/dsl-grammar.md) — every modifier and entity-level clause.
- [Type mapping](../reference/dsl-types.md) — each `.atl` type's Postgres, Go, and proto representation.
