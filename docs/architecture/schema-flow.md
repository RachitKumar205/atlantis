# Schema flow

How a schema change moves from a caller's repository into a running atlantis server.

atlantis never reads from caller repositories at runtime, and callers never write into the atlantis repository. Dev and prod differ in how the schema bytes reach the server; the invariant holds in both paths. On every `tide apply`, the server persists a new IR checkpoint to Postgres. The server loads the IR checkpoint once at startup, so a restart (or rolling restart) is required for the new schema to take effect. The key benefit is that no recompilation is needed — just a restart.

A schema change is an edit to one or more `.atl` files in a caller's repo (typically `internal/<pkg>/schema.atl`). See the [DSL reference](../reference/dsl-grammar.md) for the syntax.

## Dev path

The dev server has `ATL_MIRROR_SCHEMA=true` and `ATL_ALLOW_APPLY_MUTATION=true`.

1. Dev edits a `.atl` file in the caller repo.
2. Dev runs `tide apply` from the caller repo.
3. `tide` bundles the `.atl` files and sends `PlanSchema` over gRPC.
4. Server validates, plans, returns the plan to `tide`.
5. `tide` writes the regenerated SDK to `output_dir/`.
6. Server applies the migration to local Postgres and persists the new IR checkpoint. The dev server must be restarted for the new schema to take effect.
7. Server mirrors the received `.atl` files to `ATL_MIRROR_DIR`.

### Why the mirror

The mirror lets `tidectl codegen --schema-dir` read all caller `.atl` files from one directory without cloning each caller repo. In dev, the gRPC handler writes the received bytes to disk after each `tide apply`; in prod, CI clones each caller's repo to the same on-disk layout. Without the mirror an operator would have to assemble the caller checkouts manually before running local codegen.

## Production path — live apply (runtime dispatch)

Prod has `ATL_ALLOW_APPLY_MUTATION=true` and `ATL_MIRROR_SCHEMA=false`. Callers apply schema changes directly; the server handles migration and IR persistence in one transaction.

1. Dev opens a PR in the caller repo with an `.atl` change.
2. Caller CI runs `tide plan --against=<prod-endpoint>` on the PR. `PlanSchema` is read-only and side-effect-free; CI submits the local files in-memory and the server returns the plan without touching its database.
3. PR merges.
4. Caller CI (on merge) runs `tide apply --against=<prod-endpoint>`.
5. Server validates, applies the DDL migration to Postgres, and persists the new IR checkpoint.
6. Restart the server (a rolling restart is sufficient). The restarted server loads the new IR checkpoint and serves the updated schema.

No recompilation, no workspace manifest update. The server binary is generic and serves any entity described by the current IR.

## Production path — gated (traditional)

Prod has `ATL_ALLOW_APPLY_MUTATION=false` and `ATL_MIRROR_SCHEMA=false`. The `ApplyMigration` RPC is disabled; `PlanSchema` and `GetMergedSchema` remain available so caller-side `tide plan --against` and `tide pull` still work.

1. Dev opens a PR in the caller repo with an `.atl` change.
2. Caller-repo CI runs `tide plan --against=<prod-endpoint>`.
3. Caller PR merges.
4. A webhook (or an operator, for bootstrap and break-glass) opens a PR in the atlantis deployment repo bumping the caller's ref in `atlantis.workspace.yaml`.
5. Deployment CI on that PR:
   - Clones every caller at its pinned ref.
   - Runs `tidectl codegen` (client SDK only — no server codegen).
   - Runs `buf lint` and `buf breaking`.
   - Runs `go test` against the atlantis server itself.
6. Reviewer approves; PR merges.
7. Deploy pipeline applies the migration (`tidectl migrate-up`) and rolls the new server binary. The server loads the latest IR checkpoint from Postgres at startup.

### Compatibility contract

Step 7's order — apply migration then roll the server — only works if the new schema is backward-compatible with the old server binary. atlantis classifies each migration as additive, backfill-required, or breaking at apply time; only additive changes are safe for the migrate-then-roll order. Backfill-required and breaking changes need an explicit expand/contract sequence (apply additive parts → roll server → backfill → apply contract migration). See [migration ownership](migration-ownership.md).

In the live-apply path, the compatibility contract is similar: `tide apply` persists the new IR but the running server continues to serve the old schema until restarted. The DDL migration must be backward-compatible with the still-running server (same constraint as the traditional path). After the rolling restart, the new IR takes effect.

## The workspace manifest

`atlantis.workspace.yaml` lives at the root of the atlantis deployment repo. With runtime dispatch, the manifest is only needed for client SDK generation — the server itself loads schema from its IR checkpoint, not from the manifest. The manifest pins each caller at a git ref:

```yaml
callers:
  - name: backend
    repo: github.com/example/backend
    ref:  v1.42.0
    paths:
      - internal
  - name: data-pipeline
    repo: github.com/example/data-pipeline
    ref:  v2.7.1
    paths:
      - internal
```

`paths` lists directories — atlantis walks each recursively for `*.atl` files. The convention is to point at a tree (`internal`) rather than at individual packages.

In the traditional flow, CI clones each caller at the pinned ref, runs codegen against the union, and produces the staged migration and the client SDK. A caller bumps their pinned ref by opening a PR against this manifest. In the live-apply flow, the manifest is only updated when regenerating client SDKs — the server's schema is updated directly via `tide apply`.

## Resolving caller refs

The CI step that resolves the manifest shells out to `git clone --depth=1 --branch=<ref>`. Auth inherits from the surrounding CI environment; atlantis does not store or manage credentials.

A working-tree cache lives at `--workspace-cache` (default `.workspace-cache/`) and is reused when the ref hasn't changed.
