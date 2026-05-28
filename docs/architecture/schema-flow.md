# Schema flow

How a schema change moves from a caller's repository into a running atlantis server.

atlantis never reads from caller repositories at runtime, and callers never write into the atlantis repository. Dev and prod differ in how the schema bytes reach the server; the invariant holds in both paths. On every `tide apply`, the server persists a new IR checkpoint to Postgres with a content hash (sha256 of the IR). A PostgreSQL trigger fires `NOTIFY atl_schema_changed` after commit, and the server's schema listener hot-reloads entity metadata via atomic pointer swap. No restart is needed for field changes to existing entities; adding a brand-new entity requires a rolling restart because gRPC services are registered once at startup.

A schema change is an edit to one or more `.atl` files in a caller's repo (typically `internal/<pkg>/schema.atl`). See the [DSL reference](../reference/dsl-grammar.md) for the syntax.

## Dev path

The dev server has `ATL_MIRROR_SCHEMA=true` and `ATL_ALLOW_APPLY_MUTATION=true`.

1. Dev edits a `.atl` file in the caller repo.
2. Dev runs `tide apply` from the caller repo.
3. `tide` bundles the `.atl` files and sends `PlanSchema` over gRPC.
4. Server validates, plans, returns the plan to `tide`.
5. Server applies the migration to local Postgres and persists the new IR checkpoint. The server hot-reloads the new schema automatically via LISTEN/NOTIFY.
6. Server mirrors the received `.atl` files to `ATL_MIRROR_DIR`.
7. Dev runs `tide generate` from the caller repo to regenerate the typed Go client into `output_dir`, scoped to the namespaces the caller consumes. The generated code lives in the caller's own module — no shared SDK, no `replace` directive for the generated types. See [caller-local generation](#caller-local-sdk-generation) below.

### Why the mirror

The mirror lets `tidectl codegen --schema-dir` read all caller `.atl` files from one directory without cloning each caller repo. In dev, the gRPC handler writes the received bytes to disk after each `tide apply`; in prod, CI clones each caller's repo to the same on-disk layout. Without the mirror an operator would have to assemble the caller checkouts manually before running local codegen.

## Production path — live apply (runtime dispatch)

Prod has `ATL_ALLOW_APPLY_MUTATION=true` and `ATL_MIRROR_SCHEMA=false`. Callers apply schema changes directly; the server handles migration and IR persistence in one transaction.

1. Dev opens a PR in the caller repo with an `.atl` change.
2. Caller CI runs `tide plan --against=<prod-endpoint>` on the PR. `PlanSchema` is read-only and side-effect-free; CI submits the local files in-memory and the server returns the plan without touching its database.
3. PR merges.
4. Caller CI (on merge) runs `tide apply --against=<prod-endpoint>`.
5. Server acquires an advisory lock, validates, applies the DDL migration, and persists the new IR checkpoint with a content hash. CAS (compare-and-swap) on the content hash rejects stale applies if the checkpoint moved since planning.
6. A PostgreSQL trigger fires `NOTIFY atl_schema_changed`. The server's schema listener rebuilds entity metadata and swaps it atomically. In-flight requests complete on the old metadata; new requests see the updated schema immediately.

No restart, no recompilation, no workspace manifest update. A rolling restart is only needed when a `tide apply` introduces a brand-new entity (not just new fields). The server binary is generic and serves any entity described by the current IR.

To update the typed Go client after a schema change, the caller runs `tide generate` from its own repo (see [caller-local generation](#caller-local-sdk-generation)).

## Caller-local SDK generation

The typed Go client is generated **in the caller's own repo**, scoped to only the namespaces that caller consumes. There is no shared central SDK and no dependency on a checkout of the atlantis repo for the generated types.

`tide generate`:

1. Fetches the canonical IR from the server via the `GetCanonicalIR` admin RPC. Proto field numbers come from the server's checkpoint, so the generated wire format matches the server exactly — the caller never re-lowers `.atl` files locally (which could assign different numbers).
2. Filters the IR to the `generate:` namespaces in `tide.yaml`. A cross-namespace FK is a scalar column, so a caller that references another namespace's entity does not need that namespace's types unless it lists it in `generate:`.
3. Reads the caller's `go.mod`, computes the package prefix `<module>/<output_dir>`, and emits proto sources (scoped namespaces + the embedded `atlantis/common/v1` protos) plus typed wrappers into `output_dir`. It then runs `buf generate` for the `.pb.go` wire types and `gofmt`.

The result compiles inside the caller's module with the caller's own import paths. The only remaining `atlantis-go` dependency is the hand-written `jobs` worker runtime, for callers that run job workers — a normal library dependency, not generated code.

The central `clients/go/` SDK (generated by `tidectl codegen` + `buf generate` from the atlantis repo) still exists for atlantis's own integration tests, but callers no longer consume it.

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

In the live-apply path, `tide apply` persists the new IR and the server hot-reloads within seconds. The DDL migration must still be backward-compatible: in-flight requests may reference the old schema during the brief reload window, and callers with stale typed clients continue to send the old proto shape until they regenerate. Additive changes (new fields, new entities) are always safe; type changes and removals need the same expand/contract discipline.

## The workspace manifest

`atlantis.workspace.yaml` lives at the root of the atlantis deployment repo. With runtime dispatch, the manifest is only needed for client SDK generation — the server itself loads schema from its IR checkpoint, not from the manifest. The manifest pins each caller at a git ref:

```yaml
callers:
  - name: backend
    repo: github.com/example/backend
    ref:  v1.42.0
    paths:
      - internal
  - name: vendor-platform
    repo: github.com/example/vendor-platform
    ref:  v2.7.1
    paths:
      - internal
```

`paths` lists directories — atlantis walks each recursively for `*.atl` files. The convention is to point at a tree (`internal`) rather than at individual packages.

In the traditional flow, CI clones each caller at the pinned ref, runs codegen against the union, and produces the staged migration and the client SDK. A caller bumps their pinned ref by opening a PR against this manifest. In the live-apply flow, the manifest is only updated when regenerating client SDKs — the server's schema is updated directly via `tide apply`.

## Resolving caller refs

The CI step that resolves the manifest shells out to `git clone --depth=1 --branch=<ref>`. Auth inherits from the surrounding CI environment; atlantis does not store or manage credentials.

A working-tree cache lives at `--workspace-cache` (default `.workspace-cache/`) and is reused when the ref hasn't changed.
