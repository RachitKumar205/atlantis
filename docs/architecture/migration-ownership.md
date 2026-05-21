# Migration ownership

Atlantis's `migrations/` directory is split into two subdirectories by **who writes the files**.

## Layout

```
migrations/
├── infra/
│   ├── 0000_outbox.up.sql
│   ├── 0000_outbox.down.sql
│   └── ...
└── tidectl/
    ├── 0000_initial.up.sql
    ├── 0000_initial.down.sql
    ├── 0001_<description>.up.sql
    ├── 0001_<description>.down.sql
    └── _staged/
        ├── NNNN_tidectl_staged.up.sql
        └── NNNN_tidectl_staged.down.sql
```

`migrations/infra/` is hand-written. It holds schema for runtime machinery: the cache-invalidation outbox, the `tidectl` bookkeeping tables, anything that isn't a caller-defined entity.

`migrations/tidectl/` is codegen-emitted. `tidectl plan` writes a pair under `_staged/` named `NNNN_tidectl_staged.up.sql` / `.down.sql`; `tidectl approve` renames them into `tidectl/` at the next sequential number. Hand edits to promoted `tidectl/` files are rejected by the `codegen-check` CI step, which re-runs codegen and diffs against the working tree.

## Separate history tables

Each subdirectory maintains its own migration history:

| Dir | History table |
|---|---|
| `infra/` | `atlantis_schema_migrations_infra` |
| `tidectl/` | `atlantis_schema_migrations_tidectl` |

The histories are split by ownership, not by reference-freedom. (Caller schema may reference infra tables — e.g., triggers on entity tables write to the outbox.) Splitting the histories means `tidectl` can renumber its own migrations freely without touching `infra`'s monotonic sequence.

## Why the split

A single migration directory shared between hand-written and emitted files creates two failure modes:

- Codegen renumbers an emitted migration after a hand-written one is added, breaking the migrate tool's monotonic-version assumption.
- A hand edit to a codegen-emitted file is silently reverted on the next codegen run unless CI diffs the working tree against a fresh codegen.

## Apply order

Infra migrations are a dependency of tidectl migrations — the outbox table must exist before any entity enqueues an invalidation — so infra runs first, always.

The `tidectl` binary's `migrate-up` subcommand operates on one directory at a time. The Makefile enforces the two-history order by running `migrate-up-infra` before `migrate-up-tidectl`; both targets shell out to the `golang-migrate` binary with the appropriate `-path` and `-database` (each path using its own `x-migrations-table` so the two histories never collide).

## Adding migrations

A new hand-written infra migration: create the `NNNN_<name>.up.sql` and `.down.sql` pair under `migrations/infra/` by hand. Pick `NNNN` as the next sequential number above the existing files. Both files must be present; CI runs the up + down pair to catch missing or non-reversible down files.

A new codegen-emitted entity migration: edit an `.atl` file in a caller repo, `tide apply` runs `tidectl plan` which writes the staged pair, and `tidectl approve` promotes it to the next sequential number under `migrations/tidectl/`.

## Down migrations

Every migration in both directories ships with a `.down.sql`. `tidectl migrate-down` rolls back the most recent migration in the directory passed via `--migrations-dir`. The CI's reversibility check exercises every down to catch ones that drift out of sync with their up counterpart.
