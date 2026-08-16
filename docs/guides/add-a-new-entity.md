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

### Two policies, and only one of them is yours

`partition by` creates a pair:

| Policy | Kind | Yours to change? |
|---|---|---|
| `<table>_tenant_isolation` | `RESTRICTIVE`, compares the column to `atlantis.current_partition()` | **No.** This is the tenant boundary. |
| `<table>_default_access` | `PERMISSIVE USING (true)` | **Yes.** Replace it to add access control. |

PostgreSQL admits a row when **any** permissive policy allows it **and every**
restrictive policy allows it. The boundary is restrictive, so it ANDs with
everything — no policy you write can read outside the tenant, however it is
written. Restrictive policies only ever narrow, though, so a table needs at
least one permissive policy or it returns nothing; `<table>_default_access` is
that grant, and it is deliberately total so `partition by` behaves the same as
it always did.

To add your own access control, drop the default grant and write narrower
permissive policies:

```sql
DROP POLICY invoice_default_access ON atlantis.billing_invoice;
CREATE POLICY invoice_owner  ON atlantis.billing_invoice FOR SELECT USING (owner_id = current_user_id());
CREATE POLICY invoice_admin  ON atlantis.billing_invoice FOR ALL    USING (is_admin());
```

Those two OR together, as grants should, and both stay inside the tenant.
**Leaving the default grant in place makes narrower policies pointless** — it
already allows everything they would allow.

Your replacement survives `tide apply`. Apply re-emits the policy pair whenever
the `partition by` clause changes or the discriminator column moves, but it
creates the default grant only when the table carries no permissive policy at
all. Once your own grants are in place, apply leaves them alone. It does not put
`USING (true)` back beside them, which would allow everything they allow and
leave you reading a policy list that no longer describes who can see what.

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
pooled connection.

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
- Something must bind the tenant for the request. atlantis does: every entity
  RPC (get, batch get, query, create, update, delete), every custom query, and
  every custom procedure bind before touching the database, on both the
  dispatcher and the server `tide generate` emits.

**Set `ATL_REQUIRE_TENANT_ISOLATION=true`.** It defaults to `false`, which
downgrades the boot check to a warning. On a role that bypasses RLS — including
the superuser the default `docker-compose.yml` connects as — the policy is
inert and every read returns every tenant's rows, with nothing in the
application able to notice.

### What does not work on a partitioned entity yet

Both are refused or reported rather than silently wrong, but plan around them:

| | |
|---|---|
| **`ttl_field`** | Expired rows are not swept. The sweeper runs with no tenant bound, so its `DELETE` matches nothing. It skips the entity and increments `atlantis_sweeper_sweeps_blocked_total` — see [row TTL](row-ttl.md). |
| **Backfills** | `tide apply --backfill` refuses a partitioned entity by name. A chunked backfill has to cover every tenant, and there is no single tenant to bind. |
| **The sandbox** | Does not reproduce policy behaviour. Do not use it to test isolation. |

## Related

- [DSL grammar reference](../reference/dsl-grammar.md) — every modifier and entity-level clause.
- [Type mapping](../reference/dsl-types.md) — each `.atl` type's Postgres, Go, and proto representation.
