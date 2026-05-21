# Deploy to production

atlantis runs as a single Go binary against PostgreSQL and memcached. The order below — provision dependencies, fork the source, generate, plan migrations, build, ship, configure, run migrations, start, verify — minimizes the time the server is in a half-configured state.

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

In v0.1, atlantis ships as source. Your deployment repo is a fork of atlantis upstream that holds your `atlantis.workspace.yaml`, your generated tree, and your `migrations/tidectl/`.

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

## 3. Write the workspace manifest

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
  - name: data-pipeline
    source: git
    repo: https://github.com/example/data-pipeline.git
    ref:  v2.7.1
    paths:
      - internal
```

A caller bumps their pinned ref by opening a PR against this file in your deployment repo. Production schema changes flow only through this PR path. Caller-side `tide apply` invocations are blocked against production servers (gated in step 8).

## 4. Build `tidectl` and generate the typed surface

```
go build -o tidectl ./cmd/tidectl
./tidectl codegen --workspace=atlantis.workspace.yaml
```

`tidectl codegen` clones each caller at its pinned ref into a workspace cache at `.workspace-cache/` (gitignored), merges their `.atl` files, and overwrites `gen/`, `clients/go/`, and `atlantis/<ns>/v1/` with output keyed to your callers. The example fixtures committed in atlantis source are replaced.

## 5. Stage and approve entity migrations

```
./tidectl plan --workspace=atlantis.workspace.yaml
./tidectl approve
```

`tidectl plan` writes a staged migration under `migrations/tidectl/_staged/` and prints its classification (additive, backfill-required, or breaking). `tidectl approve` promotes it to the next numbered file under `migrations/tidectl/`.

Commit and push to your deployment repo:

```
git add atlantis.workspace.yaml gen clients atlantis migrations/tidectl
git commit -m "apply caller pins"
git push origin main
```

From now on, every change to a caller's pinned ref flows through plan + approve and a new numbered migration lands in your deployment repo.

If `tidectl plan` classifies the change as breaking, do not approve before coordinating across affected callers. (A separate breaking-change rollout guide is planned.)

## 6. Build the server binary

```
go build -o atlantis-server ./cmd/server
```

The build links your callers' entity handlers (regenerated in step 4) into a server binary tailored to this deployment. Reproducible from `atlantis.workspace.yaml` + the atlantis-source tag.

## 7. Ship the artifacts

Build a container image for image-based deploys:

```
docker build -t <your-registry>/atlantis:v0.1.0 .
docker push <your-registry>/atlantis:v0.1.0
```

For VM-based deploys, ship a tarball instead: `tar czf atlantis-v0.1.0.tgz atlantis-server tidectl migrations/`. Both `atlantis-server` and `tidectl` need to land on the host — `tidectl` runs the migration step before the server starts.

## 8. Configure and lock down schema mutation

`ATL_ALLOW_APPLY_MUTATION=false` is the single most important production setting. With it, the server rejects mutation submissions from caller `tide apply` invocations and serves only the read-only admin RPCs (plan, pull). Production schema state is whatever the workspace manifest pins; the only path to change it is a PR against the manifest, followed by a rebuild and redeploy.

Leaving `ATL_ALLOW_APPLY_MUTATION=true` in production means any caller's CI can mutate the live schema.

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
ATL_ALLOW_APPLY_MUTATION=false
LOG_LEVEL=info
```

atlantis-specific behavior toggles use the `ATL_` prefix; infra connection vars (`PG_URL`, `MEMCACHED_ADDR`, `TLS_*`, etc.) use the conventional unprefixed names. The full list is in [configuration](../reference/configuration.md).

### TLS

atlantis requires mTLS whenever TLS is enabled — there is no server-only-TLS mode. `TLS_CA_FILE` is the CA that verifies client certificates presented by callers. If you set any one of `TLS_CERT_FILE` / `TLS_KEY_FILE` / `TLS_CA_FILE`, you must set all three; partial sets are rejected at startup.

atlantis reads TLS material on startup only; there is no SIGHUP reload. To rotate, deploy new cert material and restart the server (a rolling restart is sufficient).

In an mTLS deployment, the same CA mints client certificates for every caller and for operator tooling (`grpcurl`, `grpc_health_probe`). Verification commands in step 11 and the health-check subsection assume those certs already exist.

## 9. Run migrations

```
./tidectl migrate-up --migrations-dir migrations/infra
./tidectl migrate-up --migrations-dir migrations/tidectl
```

Run before starting the new server. Apply `migrations/infra/` first (atlantis's runtime tables: outbox, bookkeeping), then `migrations/tidectl/` (your caller entity tables). The command exits non-zero if a migration fails; halt the deploy.

For zero-downtime rollouts, the migration must be backward-compatible with the still-running old version. `tidectl plan` printed the classification in step 5 — if it said `additive`, this step is safe to run before rolling the server. `backfill-required` and `breaking` need an explicit expand/contract sequence.

## 10. Start the server

Run `atlantis-server` with the environment from step 8.

## 11. Verify the deploy

After the server starts, confirm it's serving:

```
grpcurl -cacert ca.crt -cert client.crt -key client.key \
  atlantis.internal:9090 atlantis.admin.v1.Admin/GetMergedSchema
```

A success response returns the current merged schema as JSON. Anything else (TLS handshake failure, no response) indicates a problem; check the server logs.

## Logging

Logging goes to stdout as one human-readable line per event. v0.1 has no JSON log mode; a shipper like `vector` or `fluent-bit` can parse the line format if you need structured logs in your aggregator.

## Health checks

The server implements the standard [gRPC Health Checking protocol](https://grpc.io/docs/guides/health-checking/). Use [grpc_health_probe](https://github.com/grpc-ecosystem/grpc-health-probe) from a Kubernetes probe or any other orchestrator:

```
grpc_health_probe \
  -addr=localhost:9090 \
  -tls -tls-ca-cert=/etc/atlantis/ca.crt \
  -tls-client-cert=/etc/atlantis/probe.crt \
  -tls-client-key=/etc/atlantis/probe.key
```

The probe needs a client cert because mTLS is enforced on every RPC (there's no health-check exemption). A dedicated HTTP `/healthz` endpoint is not shipped; use the gRPC probe.

## Shutdown

On `SIGTERM` the server cancels its top-level context and calls gRPC `GracefulStop`, which waits for in-flight RPCs to drain before exiting. **There is no configurable grace period.** Set your orchestrator's `terminationGracePeriodSeconds` to at least the longest expected RPC latency (most workloads: 30s is conservative). Send `SIGKILL` past that.

## Backup and restore

PostgreSQL is the source of truth. Use whatever PITR or `pg_dump` strategy your operations team already runs.

Memcached holds no durable state. On total cache loss the server cold-starts against Postgres; latency is briefly elevated until the working set re-warms.

Your workspace manifest, generated tree, and `migrations/tidectl/` all live in your deployment repo. Recover the build with `git checkout <known-good-ref>` and rebuild from step 4.

## Rollback

Revert the deploy by redeploying the previous binary. The previous server version expects the previous schema, so the entity migrations must be rolled back too:

```
./tidectl migrate-down --migrations-dir migrations/tidectl
```

Do **not** run `migrate-down` against `migrations/infra` as part of a normal rollback — atlantis's runtime tables are only rolled back when downgrading atlantis itself.

A migration that drops data or changes column types is not safely reversible. Restore from PITR or `pg_dump` in that case; do not rely on the down-migration to recover data.

For schema-only reverts where the new server version isn't yet shipped, reverting the manifest PR in your deployment repo is sufficient — the manifest is the source of truth and a rebuild reproduces the prior state.

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
| `atlantis.workspace.yaml` | **your deployment repo** — pins your callers |
| `gen/`, `clients/go/`, `atlantis/<ns>/v1/` | **your deployment repo** — regenerated against your callers |
| `migrations/tidectl/` | **your deployment repo** — generated by your `tidectl plan + approve` |

The fork model is provisional. A future release will separate the operator's deployment repo from atlantis source so a single precompiled `atlantis-server` binary can be configured against the workspace at runtime, without a fork.

## Related

- [Configuration reference](../reference/configuration.md) — every env var.
- [Schema flow](../architecture/schema-flow.md) — workspace-manifest pattern in detail.
- [`tidectl` CLI reference](../reference/cli-tidectl.md) — every admin command.
