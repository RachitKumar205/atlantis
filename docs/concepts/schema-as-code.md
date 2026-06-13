# Schema as code

In atlantis, the `.atl` files in your service's git repo define your schema. The migrations, the generated client, the SQL the server emits, and the tables in PostgreSQL all derive from them.

Hasura, Supabase, and most schema-via-UI tools treat the live database as authoritative; the repo, if present, is documentation. atlantis inverts that: the `.atl` files in git are authoritative and the database is derived from them.

## What it means in practice

You add a column by editing an `.atl` file and running `tide apply`. The commit that ships the change is what makes it durable across rebuilds, redeploys, and other engineers' machines.

Every change to your schema is in git history. `git blame` on an `.atl` file shows when and by whom a column changed.

## The server is a mirror

The atlantis server holds the applied state of every schema it has received. That state mirrors what's in git but isn't authoritative on its own. If the server's state ever drifts from what the files say, the next `tide apply` brings the server back into alignment with the files.

A traditional migration tool leaves the database in whatever state the last migration produced; atlantis treats the database as a deterministic function of the input schema files. Delete an entity from a `.atl` file and `tide apply` prepares a migration to drop the table.

One case breaks the reconcile-on-apply rule. If the live database carries a bare unique index the schema doesn't declare — a `CREATE UNIQUE INDEX` with no backing constraint — `tide apply` refuses rather than reconciling — applying over it would leave a hidden constraint silently rejecting legitimate writes. Drop the index, declare the uniqueness in the `.atl`, or set `ATLANTIS_ALLOW_INDEX_DRIFT=1` to apply anyway.

## What lives where

A typical service repo:

```
my-service/
├── internal/
│   ├── notes/
│   │   └── schema.atl
│   └── users/
│       └── schema.atl
├── pb/                       # generated client code (Go shown), gitignored
│   └── app/
│       ├── note.pb.go
│       └── user.pb.go
├── tide.yaml
└── main.go
```

The `.atl` files are checked into git. The `pb/` directory is regenerated on every `tide apply` and typically `.gitignored`.

## No schema editor

There is no UI to drag-and-drop a column, and no API endpoint that adds a field outside the `tide apply` path. This is a permanent design choice.

A schema editor would let the server and the repo diverge: PRs would no longer reflect what runs in production, and `git blame` would describe a different history than the live tables.

## What the position costs

Schema-as-code has costs. Non-engineers cannot change schema without going through code review. Local experiments have to be reverted explicitly to undo them. Emergency changes still go through `tide apply`.

## Related

- [Caching and invalidation](caching-and-invalidation.md) — how the cache stays consistent with the schema it derives from.
- [`tide` vs `tidectl`](tide-vs-tidectl.md) — why only the caller CLI can apply schemas.
