# Architecture overview

A map of the Atlantis system. Each section links to a deeper page.

Atlantis has two binaries: a server and two CLIs that talk to it.

**The server** is a Go binary serving gRPC. It registers an `AdminService` (schema management — `PlanSchema`, `ApplyMigration`, `GetMergedSchema`) used by `tide` and `tidectl`, plus one service per declared entity (`<Entity>Service` with `Get`, `Query`, `Create`, `Update`, `Delete`) used by the caller's application code at runtime. These entity services are dispatched from the IR at runtime, not compiled handlers — the server reads the IR checkpoint at startup and serves any entity it describes. It also registers the standard gRPC Health Checking service and reflection. One process per Postgres database.

**`tide`** is the caller-side CLI. It runs in a caller's service repo, opens a gRPC connection to the server, and submits `.atl` files via `AdminService.PlanSchema` / `ApplyMigration`. To produce the typed Go client, the caller runs `tide generate` from its own repo, which fetches the canonical IR over `GetCanonicalIR` and emits proto + typed wrappers into the caller's module — the SDK is generated caller-local, not returned in the apply response.

**`tidectl`** is the operator-side CLI. It runs on the server host, invokes `internal/codegen/` in-process against local `.atl` files (from `--schema-dir` or a workspace manifest), writes emitted files into the server source tree, and shells out to `migrate` for database migrations. It speaks no gRPC.

The two CLIs do not call each other. They both reuse `internal/codegen/` — `tide` reaches it through the server's RPC; `tidectl` runs it directly.

## What happens on `tide apply`

The caller's `tide` reads `.atl` files under `schema_paths` and bundles them into a `PlanSchema` gRPC request. The server lexes, parses, and validates the bundle; cross-caller breaking changes are detected here. The IR runs through the codegen, which produces the SQL migration and the new IR checkpoint. The plan response also warns on a bare unique index the schema doesn't declare — a `CREATE UNIQUE INDEX` with no backing constraint.

Atomicity: the server runs the SQL migration plus a write to its own catalog tables (recording the new schema version and the caller responsible) inside one Postgres transaction. Inside that same transaction, apply re-checks unique-index drift and **refuses** if it finds any — with a `DROP INDEX` remediation — unless `ATLANTIS_ALLOW_INDEX_DRIFT=1` is set in the server's environment. On success the server persists the new IR checkpoint and hot-reloads entity metadata via `LISTEN/NOTIFY`.

There is no SDK in the apply response. To produce or refresh the typed Go client, the caller runs `tide generate` from its own repo; it fetches the canonical IR over `GetCanonicalIR` and emits proto + typed wrappers into `output_dir/`. See [codegen pipeline](codegen-pipeline.md) for the generation paths.

See: [codegen pipeline](codegen-pipeline.md), [schema flow](schema-flow.md).

## What happens on `Get<Entity>`

The caller invokes the generated client method against the entity's per-entity service. The server's handler checks the body cache; on a hit, it returns the cached body without touching Postgres. On a miss, it queries Postgres, stores the row in the cache with the entity's declared TTL, and returns it.

The cache key shape and the invalidation invariants are in [cache architecture](cache-architecture.md).

## What happens on `Create` / `Update` / `Delete<Entity>`

The three mutation RPCs share a single handler shape, dispatched at runtime from the entity's IR metadata; they differ only in the SQL statement they run. Inside one Postgres transaction, the data change commits and an invalidation row is inserted into the `outbox` table for each affected cache key. The outbox row ensures the invalidation survives a server crash between commit and the memcached write.

After commit, the outbox worker drains the queue and applies invalidations to memcached.

See: [cache architecture](cache-architecture.md), [migration ownership](migration-ownership.md).

## Where the source lives

| Component | Directory |
|---|---|
| Server entry point | `cmd/server/` |
| Caller CLI | `cmd/tide/` |
| Admin CLI | `cmd/tidectl/` |
| DSL lexer, parser, IR | `internal/dsl/` |
| Codegen (proto, Go, SQL) | `internal/codegen/` |
| Postgres connection, transactions, outbox writer | `internal/storage/pg/` |
| Cache client and outbox drainer | `internal/cache/` |
| Runtime helpers linked into generated code | `internal/runtime/` |
| In-process sandbox runtime (sim + embedded backends) | `internal/runtime/sandbox/` |
| Console BFF (incl. `/api/sandbox/*` layer) | `internal/console/` |
| Admin service handlers | `internal/server/admin/` |
| Generated SDK module | `clients/go/` |
| Hand-written infra migrations | `migrations/infra/` |
| Codegen-emitted migrations | `migrations/tidectl/` |

## See also

- [Codegen pipeline](codegen-pipeline.md) — `.atl` → IR → proto, Go, SQL.
- [Cache architecture](cache-architecture.md) — body cache, query-result cache, outbox worker, version pointers.
- [Schema flow](schema-flow.md) — dev (`tide apply` mirror) vs prod (workspace manifest).
- [Migration ownership](migration-ownership.md) — `infra/` vs `tidectl/` split.
- [The sandbox](../concepts/sandbox.md) — in-process disposable runtime hosted by the console BFF.
