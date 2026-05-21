# `tide` vs `tidectl`

Atlantis ships two CLIs: `tide` for callers and `tidectl` for operators. The split keeps the caller surface narrow — `tide` can submit schema changes but cannot roll back migrations, run destructive DDL, or touch another service's schema.

## What `tide` does

`tide` runs from your service repo. It reads the project's `tide.yaml`, collects the `.atl` schema files it points at, and submits them to the Atlantis server. It submits schema (`apply`), previews submissions (`plan`), and resyncs from server state (`pull`).

## What `tidectl` does

`tidectl` runs on a host with direct access to the Atlantis server's database and migration directory. It owns codegen, migration application, and the approval flow for migrations staged by caller submissions.

## Why the split

Caller CI runs on every PR across every service repo. The blast radius of a buggy or malicious `tide apply` has to be bounded: no caller can drop tables, roll back history, or run codegen for someone else's schema. Destructive operations live in `tidectl`, which runs server-side and is not invoked from caller pipelines.

## Related

- [Schema as code](schema-as-code.md) — why the caller CLI is the only path that mutates schema.
- [`tide` CLI reference](../reference/cli-tide.md)
- [`tidectl` CLI reference](../reference/cli-tidectl.md)
