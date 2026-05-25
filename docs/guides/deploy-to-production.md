# Deploy to production

atlantis runs as a single Go binary against PostgreSQL and memcached. The server reads schema IR at startup and dispatches entity CRUD at runtime — there is no codegen step for the server and no compiled entity handlers. The order below — provision dependencies, fork the source, configure, run migrations, start, verify — minimizes the time the server is in a half-configured state.

## 1. Provision PostgreSQL and memcached

A managed Postgres (15 or later) and a managed memcached are both fine.

For Postgres, create the database and a role with enough grants to manage its own schema:

```sql
CREATE ROLE atlantis LOGIN PASSWORD '<secret>';
CREATE DATABASE atlantis OWNER atlantis;
\c atlantis
GRANT ALL ON SCHEMA public TO atlantis;
-- If your callers declare entities that use pgvector or citext, install the extensions:
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS citext;
```

For memcached, one or many nodes both work. atlantis distributes keys across the addresses listed in `MEMCACHED_ADDR`. Start with a single small node and scale based on observed hit rate.

## 2. Fork atlantis source into your deployment repo

In v0.1, atlantis ships as source. Your deployment repo is a fork of atlantis upstream that holds the server binary source and your `migrations/tidectl/`. The fork no longer holds generated entity code — the server loads schema IR at startup and handles entity CRUD via runtime dispatch.

First, create an empty private repo (e.g. `github.com/your-org/atlantis-deploy`). Then:

```
git clone --branch v0.1.0 https://github.com/rachitkumar205/atlantis my-atlantis
cd my-atlantis
git remote remove origin
git remote add origin git@github.com:your-org/atlantis-deploy.git
git remote add upstream https://github.com/rachitkumar205/atlantis
git push -u origin v0.1.0
```

The `upstream` remote is what you'll fetch from to pull future atlantis releases. From here on, everything you commit lands in your deployment repo, not in atlantis upstream.

See [What lives where](#what-lives-where) at the bottom for the upstream-vs-fork ownership map.

## 3. Write the workspace manifest (optional — client SDK only)

The workspace manifest is still useful if you want to generate client SDKs (proto definitions + typed Go wrappers) for your callers. The server itself does not read or need this file — it loads schema IR from its checkpoint at startup.

Replace the contents of `atlantis.workspace.yaml` with your callers, each pinned at a git ref:

```yaml
version: 1
callers:
  - name: backend
    source: git
    repo: https://github.com/example/backend.git
    ref:  v1.42.0
    paths:
      - internal
  - name: vendor-platform
    source: git
    repo: https://github.com/example/vendor-platform.git
    ref:  v2.7.1
    paths:
      - internal
```

To regenerate client SDKs after a caller changes their `.atl` files:

```
go build -o tidectl ./cmd/tidectl
./tidectl codegen --workspace=atlantis.workspace.yaml
```

`tidectl codegen` clones each caller at its pinned ref, merges their `.atl` files, and overwrites `clients/go/` and the proto definitions under `atlantis/<ns>/v1/`. It no longer produces `gen/go/server/` — entity CRUD is handled by runtime dispatch.

## 4. Build the server binary

```
go build -o atlantis-server ./cmd/server
```

The binary is generic — it contains no caller-specific entity handlers. The same binary works for any set of callers; schema is loaded from the IR checkpoint at startup.

## 5. Ship the artifacts

Build a container image for image-based deploys:

```
docker build -t <your-registry>/atlantis:v0.1.0 .
docker push <your-registry>/atlantis:v0.1.0
```

For VM-based deploys, ship a tarball instead: `tar czf atlantis-v0.1.0.tgz atlantis-server tidectl migrations/`. Both `atlantis-server` and `tidectl` need to land on the host — `tidectl` runs the migration step before the server starts.

## 6. Configure schema mutation policy

With runtime dispatch, the server can accept live schema changes from callers via `tide apply`. The `ATL_ALLOW_APPLY_MUTATION` flag controls this:

- **`false` (locked-down mode)** — the server rejects mutation submissions from caller `tide apply`. Schema changes require a deploy: update `atlantis.workspace.yaml`, regenerate migrations with `tidectl plan` + `tidectl approve`, redeploy. This is the traditional flow.
- **`true` (live-apply mode)** — callers run `tide apply` directly against the production server. The server plans, migrates, and persists the new IR checkpoint to Postgres. A server restart (or rolling restart) is required for the new schema to take effect — the server loads the IR checkpoint once at startup. The key benefit is that no recompilation is needed; just restart.

The tradeoff: `ATL_ALLOW_APPLY_MUTATION=true` means any caller with network access and valid mTLS credentials can mutate the live schema. Gate this with caller-identity restrictions and CI-only apply policies (see the schema change workflow below).

A minimal production environment:

```
PG_URL=postgres://atlantis@db.internal:5432/atlantis?sslmode=require
MEMCACHED_ADDR=memcache-0.internal:11211,memcache-1.internal:11211
GRPC_LISTEN=:9090
TLS_CERT_FILE=/etc/atlantis/tls.crt
TLS_KEY_FILE=/etc/atlantis/tls.key
TLS_CA_FILE=/etc/atlantis/ca.crt
AUTO_MIGRATE=false
ATL_MIRROR_SCHEMA=false
ATL_ALLOW_APPLY_MUTATION=true
LOG_LEVEL=info
```

atlantis-specific behavior toggles use the `ATL_` prefix; infra connection vars (`PG_URL`, `MEMCACHED_ADDR`, `TLS_*`, etc.) use the conventional unprefixed names. The full list is in [configuration](../reference/configuration.md).

### TLS

atlantis requires mTLS whenever TLS is enabled — there is no server-only-TLS mode. `TLS_CA_FILE` is the CA that verifies client certificates presented by callers. If you set any one of `TLS_CERT_FILE` / `TLS_KEY_FILE` / `TLS_CA_FILE`, you must set all three; partial sets are rejected at startup.

atlantis reads TLS material on startup only; there is no SIGHUP reload. To rotate, deploy new cert material and restart the server (a rolling restart is sufficient).

In an mTLS deployment, the same CA mints client certificates for every caller and for operator tooling (`grpcurl`, `grpc_health_probe`). Verification commands in step 9 and the health-check subsection assume those certs already exist.

## 7. Run migrations

```
./tidectl migrate-up --migrations-dir migrations/infra
./tidectl migrate-up --migrations-dir migrations/tidectl
```

Run before starting the new server. Apply `migrations/infra/` first (atlantis's runtime tables: outbox, bookkeeping), then `migrations/tidectl/` (your caller entity tables). The command exits non-zero if a migration fails; halt the deploy.

For zero-downtime rollouts, the migration must be backward-compatible with the still-running old version. `tidectl plan` printed the classification — if it said `additive`, this step is safe to run before rolling the server. `backfill-required` and `breaking` need an explicit expand/contract sequence.

## 8. Start the server

Run `atlantis-server` with the environment from step 6. At startup the server loads the latest IR checkpoint from Postgres and builds entity metadata for runtime dispatch. No caller-specific code is compiled in — the server handles any entity described by the IR.

## 9. Verify the deploy

After the server starts, confirm it's serving:

```
grpcurl -cacert ca.crt -cert client.crt -key client.key \
  atlantis.internal:9090 atlantis.admin.v1.Admin/GetMergedSchema
```

A success response returns the current merged schema as JSON. Anything else (TLS handshake failure, no response) indicates a problem; check the server logs.

## Schema change workflow (runtime dispatch)

With `ATL_ALLOW_APPLY_MUTATION=true`, schema changes no longer require a server rebuild. The workflow:

1. Developer edits `.atl` files in the caller repo and opens a PR.
2. Caller CI runs `tide plan --against=<prod-endpoint>` on the PR. The server returns the plan without touching its database — this is read-only.
3. PR merges.
4. Caller CI (on merge to main) runs `tide apply --against=<prod-endpoint>`.
5. The server validates, applies the migration to Postgres, and persists the new IR checkpoint.
6. Restart the server (a rolling restart is sufficient). The restarted server loads the new IR checkpoint and serves the updated schema.
7. No recompilation, no workspace manifest update.

For teams that want gated rollout, keep `ATL_ALLOW_APPLY_MUTATION=false` and use the traditional workspace-manifest flow: bump the caller ref in `atlantis.workspace.yaml`, run `tidectl plan` + `tidectl approve` in CI, then redeploy.

## Logging

JSON is the default log format. All levels except `debug` emit structured JSON to stdout. Debug level (`LOG_LEVEL=debug`) switches to human-readable text for local readability. No additional log shipper is needed for structured ingestion at info level and above.

## Health checks

The server implements the standard [gRPC Health Checking protocol](https://grpc.io/docs/guides/health-checking/). Use [grpc_health_probe](https://github.com/grpc-ecosystem/grpc-health-probe) from a Kubernetes probe or any other orchestrator:

```
grpc_health_probe \
  -addr=localhost:9090 \
  -tls -tls-ca-cert=/etc/atlantis/ca.crt \
  -tls-client-cert=/etc/atlantis/probe.crt \
  -tls-client-key=/etc/atlantis/probe.key
```

The probe needs a client cert because mTLS is enforced on every RPC (there's no health-check exemption). atlantis does not expose a dedicated HTTP `/healthz` endpoint; use the gRPC probe.

## Shutdown

On `SIGINT` or `SIGTERM` the server cancels its top-level context and calls gRPC `GracefulStop`, which waits for in-flight RPCs to drain before exiting. The grace period is hardcoded at 15 seconds; after that the server forces a stop. Set your orchestrator's `terminationGracePeriodSeconds` to at least 20 seconds to allow the server's own timeout to fire before the orchestrator sends `SIGKILL`.

## Backup and restore

PostgreSQL is the source of truth. Use whatever PITR or `pg_dump` strategy your operations team already runs.

Memcached holds no durable state. On total cache loss the server cold-starts against Postgres; latency is briefly elevated until the working set re-warms.

Your workspace manifest and `migrations/tidectl/` live in your deployment repo. Recover the build with `git checkout <known-good-ref>` and rebuild from step 4.

## Rollback

**Runtime dispatch flow** (`ATL_ALLOW_APPLY_MUTATION=true`): run `tide rollback --to=<version>` against the production server. The server creates a new schema version whose IR matches the target, applies the reverse migration, and persists the rolled-back IR checkpoint. Restart the server (or rolling-restart) for the rollback to take effect.

**Traditional flow** (`ATL_ALLOW_APPLY_MUTATION=false`): revert the deploy by redeploying the previous binary. The entity migrations must be rolled back too:

```
./tidectl migrate-down --migrations-dir migrations/tidectl
```

Do **not** run `migrate-down` against `migrations/infra` as part of a normal rollback — atlantis's runtime tables are only rolled back when downgrading atlantis itself.

A migration that drops data or changes column types is not safely reversible. Restore from PITR or `pg_dump` in that case; do not rely on the down-migration to recover data.

## Recommended sizing

A typical mixed read/write workload of ~1k RPS fits on a 1 vCPU / 1 GiB instance. Postgres connection saturation is the usual first bottleneck; tune `pgxpool` settings via `PG_URL` query parameters (`pool_max_conns`, `pool_min_conns`) and Postgres `max_connections` together. As a starting relationship:

```
pool_max_conns × replica_count ≤ postgres.max_connections − reserved
```

Where `reserved` covers connections you need for other things (admin sessions, replicas, etc.).

For larger workloads, scale atlantis horizontally; the server is stateless and any instance can serve any request. Memcached scales by adding nodes to `MEMCACHED_ADDR`.

## What lives where

| Path | Ownership |
|---|---|
| `cmd/`, `internal/`, `Makefile`, `Dockerfile`, atlantis source | atlantis upstream — you pull updates from `upstream` |
| `migrations/infra/` | atlantis upstream — atlantis's own runtime schema |
| `atlantis.workspace.yaml` | **your deployment repo** — pins your callers (client SDK generation only) |
| `clients/go/`, `atlantis/<ns>/v1/` | **your deployment repo** — regenerated proto + client SDK against your callers |
| `migrations/tidectl/` | **your deployment repo** — generated by `tidectl plan + approve` (traditional flow only) |

`gen/go/server/` no longer exists — entity CRUD is handled by runtime dispatch from the IR checkpoint. The server binary is generic and does not contain caller-specific generated code.

The fork model is provisional. A future release will separate the operator's deployment repo from atlantis source so a single precompiled `atlantis-server` binary can be configured against the workspace at runtime, without a fork.

## Related

- [Configuration reference](../reference/configuration.md) — every env var.
- [Schema flow](../architecture/schema-flow.md) — workspace-manifest pattern in detail.
- [`tidectl` CLI reference](../reference/cli-tidectl.md) — every admin command.
