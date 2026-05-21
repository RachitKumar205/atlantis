# Schema flow

How a schema change moves from a caller's repository into a running Atlantis server.

atlantis never reads from caller repositories at runtime, and callers never write into the atlantis repository. Dev and prod differ in how the schema bytes reach the server; the invariant holds in both paths.

A schema change is an edit to one or more `.atl` files in a caller's repo (typically `internal/<pkg>/schema.atl`). See the [DSL reference](../reference/dsl-grammar.md) for the syntax.

## Dev path

The dev server has `ATL_MIRROR_SCHEMA=true` and `ATL_ALLOW_APPLY_MUTATION=true`.

1. Dev edits a `.atl` file in the caller repo.
2. Dev runs `tide apply` from the caller repo.
3. `tide` bundles the `.atl` files and sends `PlanSchema` over gRPC.
4. Server validates, plans, returns the plan to `tide`.
5. `tide` writes the regenerated SDK to `output_dir/`.
6. Server applies the migration to local Postgres, then mirrors the received `.atl` files to `ATL_MIRROR_DIR`.

### Why the mirror

The server's hot-reload watcher only reads its own filesystem. The mirror lets dev `tide apply` reuse the same downstream path as prod: in dev the gRPC handler writes the received bytes to disk; in prod, CI clones each caller's repo to the same on-disk layout. Without the mirror the dev server would have to read from caller checkouts at runtime.

## Production path

Prod has `ATL_ALLOW_APPLY_MUTATION=false` and `ATL_MIRROR_SCHEMA=false`. The `ApplyMigration` RPC is disabled; `PlanSchema` and `GetMergedSchema` remain available so caller-side `tide plan --against` and `tide pull` still work.

1. Dev opens a PR in the caller repo with an `.atl` change.
2. Caller-repo CI runs `tide plan --against=<prod-endpoint>`. `PlanSchema` is read-only and side-effect-free; CI submits the local files in-memory and the server returns the plan without touching its database.
3. Caller PR merges.
4. A webhook (or an operator, for bootstrap and break-glass) opens a PR in the Atlantis schema repo bumping the caller's ref in `atlantis.workspace.yaml`.
5. Atlantis CI on that PR:
   - Clones every caller at its pinned ref.
   - Runs `tidectl codegen`.
   - Runs `make codegen-check` (regenerates and asserts no diff).
   - Runs `buf lint` and `buf breaking`.
   - Runs `go test` against the Atlantis server itself.
6. Reviewer approves; PR merges.
7. Deploy pipeline applies the migration (`tidectl migrate-up`) and rolls the new server binary.

### Compatibility contract

Step 7's order — apply migration then roll the server — only works if the new schema is backward-compatible with the old server binary. Atlantis classifies each migration as additive, backfill-required, or breaking at apply time; only additive changes are safe for the migrate-then-roll order. Backfill-required and breaking changes need an explicit expand/contract sequence (apply additive parts → roll server → backfill → apply contract migration). See [migration ownership](migration-ownership.md).

## The workspace manifest

`atlantis.workspace.yaml` lives at the root of the Atlantis schema repo. It pins each caller at a git ref:

```yaml
callers:
  - name: backend
    repo: github.com/example/backend
    ref:  v1.42.0
    schema_paths:
      - internal
  - name: vendor-platform
    repo: github.com/example/vendor-platform
    ref:  v2.7.1
    schema_paths:
      - internal
```

`schema_paths` lists directories — Atlantis walks each recursively for `*.atl` files. The convention is to point at a tree (`internal`) rather than at individual packages.

The Atlantis CI clones each caller at the pinned ref, runs codegen against the union, and produces the staged migration and the SDK module. A caller bumps their pinned ref by opening a PR against this manifest.

## Resolving caller refs

The CI step that resolves the manifest shells out to `git clone --depth=1 --branch=<ref>`. Auth inherits from the surrounding CI environment; Atlantis handles no credentials.

A working-tree cache lives at `--workspace-cache` (default `.workspace-cache/`) and is reused when the ref hasn't changed.
