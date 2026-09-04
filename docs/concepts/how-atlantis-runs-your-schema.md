# The apply path

A `.atl` file in your repository reaches a serving API through a
checkpoint, an apply, and a hot reload. Each step carries a guarantee.

## The checkpoint

atlantis never reads your repository. What it holds is the **checkpoint**: a
single canonical record of every caller's merged schema, with a content
hash. A `tide apply` that changes schema moves the checkpoint forward with
the migration it runs, under an advisory lock, so concurrent applies from
different repositories serialise rather than interleave.

The hash is the schema version's identity. An apply whose plan depends on
schema that moved after the plan was computed is refused with a stale
error and re-plans; an unrelated caller's apply does not invalidate your
plan. Nothing merges silently.

## What an apply is

`tide apply` submits your `.atl` files. The server parses them, merges them
with every other caller's recorded schema, computes the diff against the
checkpoint, and generates the SQL itself. The migration request carries no
SQL: it carries declarations and a plan id that hashes their content, and
what executes is what the server's generator emits for exactly those
bytes. (SQL you author inside `query` and `procedure` declarations travels
as part of the declaration and is validated before it is accepted.)
Approvals and rehearsal verdicts bind to the plan id, so a decision about
one version of a change never carries over to another.

Before any DDL runs, the apply passes the composed gate — change policy,
the caller's apply-policy tier, protected entities, freeze windows — inside
the same locked transaction. See [Change approval](change-approval.md).

## Hot reload

The checkpoint commit notifies the server, which reloads its entity
metadata: in-flight requests finish on the old schema, and new requests
see the new one moments after the commit. No restart, no redeploy.

One exception: a brand-new entity, custom query, or procedure adds a gRPC
method, and methods register at server startup. The declaration is
persisted and visible to `tide show` at once; its RPC becomes callable when
your organisation's server next restarts.

## Migration ownership

atlantis owns the DDL. You declare the shape; the generator derives the
migration, classifies it, and the gate decides whether it runs unattended.
There are no migration files in your repository and nothing to run by hand
— the schema history in the console and `tide history` is the record of
what ran and when.

- Rolling back is a schema change too: it generates a forward migration
  from the recorded snapshots and records a new version. The console's
  Operations page executes it; `tide rollback --to=<version> --dry-run`
  previews it.
- Anything changed in the database outside atlantis is **drift**.
  `tide inspect` reports it; `tide apply` refuses over conflicting
  unique-index, `check`-constraint, or column-type drift, naming the
  object and the remediation.

## Where the boundaries are

- Each organisation runs on its own server and database cluster: your
  schema shares nothing with other organisations.
- Destructive migrations park objects for 30 days instead of dropping them;
  [recovery is a revert](../guides/recover-a-dropped-table.md).
- The stale-read window after a cached write is about 250 milliseconds;
  see [Caching and invalidation](caching-and-invalidation.md).
- The generated client is decoupled from the server's release: proto field
  numbers are preserved by field name across applies, so an old client
  keeps working against a newer schema. See
  [The generated client](the-generated-client.md).

## Related

- [Schema as code](schema-as-code.md) — why the repository is the source of
  truth.
- [Schema versioning](schema-versioning.md) — the version registry, blame,
  and staleness.
- [Change approval](change-approval.md) — the gate in detail.
