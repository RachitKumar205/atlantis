# Local development

After this recipe you'll have atlantis running on your laptop against a local Postgres, picking up uncommitted `.atl` changes from sibling caller repos.

Prereqs:

- A Postgres instance the operator can reach. A local snapshot of staging or prod is the common case; see [Adopt an existing database](adopt-an-existing-database.md) for the snapshot + restore path.
- A memcached instance on `localhost:11211` (or wherever; the address is configurable). `docker run -d -p 11211:11211 memcached:1.6-alpine` is enough.
- `buf` and `go` (1.25+) on `$PATH`.
- One or more caller repos with `.atl` files. They don't need to be committed.

## 1. Write `atlantis.dev.yaml`

In the atlantis repo root:

```yaml
version: 1
callers:
  - name: api
    source: local
    path: ../api
    paths:
      - internal/auth/schema.atl
      - internal/orders/schema.atl
  - name: data-pipeline
    source: local
    path: ../data-pipeline
    paths:
      - internal/auth/schema.atl
      - internal/jobs/schema.atl
```

- `source: local` reads the working tree. No `git clone`, no commit required. Edit a `.atl`, the next codegen run picks it up.
- `path:` resolves against the manifest's own directory. `../api` works regardless of where you invoke `tidectl dev` from.
- `paths:` is a flat list of `.atl` files relative to the caller's `path`. The manifest is auditable — there is no globbing. Add a row when you add a schema file.

For mixed setups (one caller pinned via git, one local), each row independently picks its source kind. `source: git` callers still need `repo:` and `ref:`.

## 2. Run `tidectl dev`

```bash
PG_URL="postgres://atlantis:atlantis@localhost:5432/atlantis" \
MEMCACHED_ADDR="localhost:11211" \
ATL_ALLOW_APPLY_MUTATION=true \
  tidectl dev
```

1. `tidectl codegen --workspace=atlantis.dev.yaml` — walks each caller's `.atl` files, lowers them into one IR, writes `proto/` and `gen/go/` (server, client, keys).
2. `buf lint && buf generate` — writes Go protobuf code under `clients/go/pb/` from the regenerated `.proto` tree.
3. `go build -o ./bin/atlantis ./cmd/server` — rebuilds the server with the freshly-generated entity stubs linked in.
4. Execs `./bin/atlantis` with the environment passed through. `Ctrl+C` forwards to the child, triggering atlantis's graceful-shutdown path (outbox drain, gRPC `GracefulStop`).

To iterate: edit a `.atl`, `Ctrl+C` the server, re-run `tidectl dev`.

This flow rebuilds the **server** (and the central `clients/go/` SDK used by atlantis's own tests). It is separate from how a caller gets its typed client: a caller runs [`tide generate`](../reference/cli-tide.md#tide-generate) from its own repo to emit a scoped client into its module. Server-side runtime dispatch means the server never needs the generated client — only callers do.

## Flags worth knowing

- `--workspace <path>` — non-default manifest location. Useful if you keep two manifests (`atlantis.dev.yaml`, `atlantis.dev-local-only.yaml`) for different setups.
- `--skip-build` — exec the existing `./bin/atlantis` without re-running codegen / buf / go build. Use when you only want to restart the server after an env-var change.
- `--skip-buf` — re-run codegen + go build but skip `buf lint` and `buf generate`. Useful when the proto tree is current but you tweaked entity-emitter code.
- `--bin <path>` — write the binary somewhere other than `./bin/atlantis`. Lets you keep multiple builds (`./bin/atlantis-dev`, `./bin/atlantis-prod`).

## Production vs dev

`tidectl dev` is **for local iteration only**. Production deployments use:

- `atlantis.workspace.yaml` (note: not `atlantis.dev.yaml`) with `source: git`.
- Each caller pinned at a tag or full SHA.
- CI runs `tidectl codegen --workspace=atlantis.workspace.yaml`, then `make build`, then ships the binary.

The two manifests can coexist in the same atlantis deployment repo. Commit `atlantis.workspace.yaml`; gitignore `atlantis.dev.yaml` (its `path:` values are operator-specific).

## Common errors

- `atlantis.dev.yaml not found` — create the manifest at the repo root, or pass `--workspace <path>`.
- `caller api: local path ../api: stat ...: no such file or directory` — the path in the manifest doesn't exist on disk. Check the relative path resolves against the manifest's directory, not your shell's cwd.
- `caller api: path is not allowed for source: git` — you set both `path:` and `repo:`/`ref:` on one caller. Pick one mode per row.
- `pg pool init: ...` from the server — `PG_URL` is wrong or Postgres isn't reachable.
- `memcached: ...` from the server — `MEMCACHED_ADDR` is wrong, or memcached isn't running.
- `permission denied for table ...` — the role in `PG_URL` doesn't have grants on the caller schemas. See [Adopt an existing database](adopt-an-existing-database.md) §3 for the grant SQL.

## Related

- [Adopt an existing database](adopt-an-existing-database.md) — provisioning the local Postgres clone atlantis runs against.
- [Deploy to production](deploy-to-production.md) — the prod-shaped workflow with `source: git` and pinned refs.
- [DSL grammar reference](../reference/dsl-grammar.md) — what goes inside the `.atl` files atlantis reads.
- [Use the sandbox](use-the-sandbox.md) — disposable copies of the merged schema for testing queries against seeded data.
