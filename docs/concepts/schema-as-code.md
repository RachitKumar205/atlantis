# Schema as code

The `.atl` files in your service's git repository define your schema. The
migrations, the generated client, the SQL the server emits, and the tables
in PostgreSQL all derive from them; the database is a function of the files,
not the other way around.

## Editing a schema

You add a column by editing an `.atl` file and running `tide apply`. The
apply changes the database; the commit makes the declaration reproducible
across rebuilds, redeploys, and other engineers' machines.

Every change to your schema is in git history. `git blame` on an `.atl`
file shows when and by whom a column changed — and the platform's own
[schema versioning](schema-versioning.md) records the same provenance on
the applied side.

## The server is a mirror

The server holds the applied state of every schema it has received. That
state mirrors what's in git but isn't authoritative on its own: if the two
ever disagree, the next `tide apply` brings the server back into alignment
with the files. Delete an entity from a `.atl` file and `tide apply`
prepares a migration to drop the table — parked for 30 days, not destroyed.

One case is refused instead of reconciled. A live object that conflicts
with the declaration — a bare unique index the schema doesn't declare, a
diverged `check`, a column whose type moved — is **drift**, and applying
over it would hide a constraint or discard an intentional change.
`tide apply` names the object and the remediation;
[`tide inspect`](../reference/cli-tide.md#tide-inspect) reports all drift
in one pass.

## What lives where

A typical service repository:

```
my-service/
├── internal/
│   ├── notes/
│   │   └── schema.atl
│   ├── users/
│   │   └── schema.atl
│   └── atlantis/             # generated client — committed
│       ├── atlantis/
│       ├── pb/
│       ├── client/
│       ├── buf.gen.yaml
│       ├── buf.yaml
│       └── tide.manifest.json
├── tide.yaml
└── main.go
```

Both the `.atl` files and the generated client are committed: a schema
change and the API change it causes review together. See
[The generated client](the-generated-client.md).

## No schema editor

The console does not edit schema. There is no UI to drag-and-drop a
column; day-to-day changes enter through `tide apply` from a repository,
and the console shows the current schema, what changed and when, who
applied it, and what waits on approval. Two admin-gated console actions do
write schema state, both recorded in the history: rolling back to an
earlier version, and baselining an imported database during
[adoption](../guides/adopt-an-existing-database.md). Neither invents a
shape that was not already declared.

## Constraints this places on you

- Day-to-day schema changes travel through code review.
- A local experiment is undone by reverting it, explicitly.
- Emergency changes still travel through `tide apply` — with the
  [override path](change-approval.md#overrides) when the gate is in the
  way.

## Related

- [The apply path](how-atlantis-runs-your-schema.md) — the checkpoint, the
  apply, hot reload.
- [Schema versioning](schema-versioning.md) — the applied-side history.
- [Caching and invalidation](caching-and-invalidation.md) — how the cache
  stays consistent with the schema it derives from.
