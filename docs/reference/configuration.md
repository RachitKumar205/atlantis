# Configuration

Environment variables read by the Atlantis server. The full list lives in `cmd/server/config.go`; `.env.example` ships with defaults that match this page.

## Required

`PG_URL` — PostgreSQL connection string in libpq URL format. The only variable without a default; the server exits at startup if it's unset.

## Reloading and precedence

All variables are read once at startup. Changes require a restart; `SIGHUP` is not handled.

## gRPC listener

| Variable | Default | Notes |
|---|---|---|
| `GRPC_LISTEN` | `:9090` | Address the gRPC server binds to. |
| `TLS_CERT_FILE` | (unset) | Server's TLS certificate. |
| `TLS_KEY_FILE` | (unset) | Server's TLS key. |
| `TLS_CA_FILE` | (unset) | CA certificate for verifying client certs (mTLS). |

The three TLS variables must be set together or all left empty. A partial set is rejected at startup. The server does not support TLS without client-certificate verification; mTLS is mandatory whenever TLS is enabled. With all three empty, TLS is disabled (dev only).

## Postgres pool

| Variable | Default |
|---|---|
| `PG_MAX_CONNS` | `50` |
| `PG_MIN_CONNS` | `10` |
| `PG_MAX_CONN_IDLE` | `5m` |
| `PG_MAX_CONN_LIFETIME` | `1h` |
| `PG_HEALTHCHECK_PERIOD` | `30s` |
| `PG_QUERY_TIMEOUT_DEFAULT` | `2s` |

Durations use Go syntax (`5m`, `30s`, `1h`, `500ms`).

`PG_QUERY_TIMEOUT_DEFAULT` is the default per-query context deadline applied at the storage layer. A query that exceeds it is cancelled via Postgres `statement_timeout`. Per-RPC override is not yet supported in v0.1.

## Memcached

| Variable | Default | Notes |
|---|---|---|
| `MEMCACHED_ADDR` | `localhost:11211` | Comma-separated for multiple nodes. |
| `MEMCACHED_TIMEOUT` | `100ms` | Per-operation timeout. |

## Cache

| Variable | Default | Notes |
|---|---|---|
| `CACHE_LRU_SIZE` | `1024` | In-process LRU size in front of memcached. |
| `CACHE_DEFAULT_TTL` | `10m` | Default TTL when an entity's `cache { ... }` block does not declare `ttl=`. |
| `CACHE_XFETCH_BETA` | `1.0` | Probabilistic early-refresh beta (Vattani et al. 2015). `0` disables; `1.0` is the canonical default; values much above `2` cause aggressive refresh. |

## Outbox worker

| Variable | Default | Notes |
|---|---|---|
| `OUTBOX_BATCH_SIZE` | `100` | Rows processed per worker tick. |
| `OUTBOX_DRAIN_INTERVAL` | `250ms` | Time between worker ticks. |
| `OUTBOX_ALERT_LAG` | `5m` | The worker emits a warning log line when the oldest unprocessed row is older than this. |
| `OUTBOX_POINTER_TTL` | `24h` | TTL on body-cache pointer keys. |

## Rate limiting

| Variable | Default | Notes |
|---|---|---|
| `RATE_LIMIT_DEFAULT_QPS` | `1000` | Per-caller QPS when no override applies. |
| `RATE_LIMIT_BURST` | `200` | Token-bucket burst capacity. |
| `RATE_LIMIT_PER_CALLER` | (unset) | Comma-separated `caller=qps` overrides. |
| `RATE_LIMIT_SATURATION_CUTOFF` | `0.80` | Acquired-conns / max-conns ratio at which low-priority RPCs start shedding. |

`RATE_LIMIT_PER_CALLER` format: `caller1=qps1,caller2=qps2`. Whitespace around tokens is trimmed. Pairs where the QPS does not parse as a positive integer, or where the `caller=` form is malformed, are silently dropped. Check startup logs to confirm the parsed map.

## Migrations

| Variable | Default | Notes |
|---|---|---|
| `AUTO_MIGRATE` | `false` | Apply pending migrations on boot. |
| `MIGRATIONS_DIR` | `migrations` | Directory passed to the bundled migrate runner. Resolved relative to the server's working directory. |

Production should leave `AUTO_MIGRATE=false` and run migrations explicitly as a deploy step.

## Admin RPC gating

`ATL_ALLOW_APPLY_MUTATION` is the kill switch for the `ApplyMigration` RPC. When `false`, the server rejects mutation submissions; the plan and pull RPCs remain available so caller-side validation still works.

| Variable | Default | Notes |
|---|---|---|
| `ATL_ALLOW_APPLY_MUTATION` | `false` | Gates the `ApplyMigration` RPC. Must be `false` in production. |

## Schema mirror (dev only)

These variables enable the local-development workflow where the server mirrors caller-submitted `.atl` files to disk so a file watcher can react to schema changes. Both must be `false` in production.

| Variable | Default | Notes |
|---|---|---|
| `ATL_MIRROR_SCHEMA` | `false` | When `true`, the server writes each successful `ApplyMigration` submission to `ATL_MIRROR_DIR`, partitioned by caller. |
| `ATL_MIRROR_DIR` | `schema` | Destination for mirrored caller files. Ignored when `ATL_MIRROR_SCHEMA=false`. |

## Logging

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |

## Parsing

Booleans accept `1`, `true`, `yes`, `on` (case-insensitive) for true and `0`, `false`, `no`, `off` for false.

All typed variables (int, float, duration, bool) silently fall back to their default on parse error. `PG_MAX_CONNS=fifty` produces `50` with no warning. Check the startup config dump to confirm the parsed value.

## Shutdown

The server installs `SIGINT` / `SIGTERM` handlers that cancel a top-level context and call gRPC `GracefulStop`. Shutdown blocks until in-flight RPCs return. Supervisors should issue `SIGKILL` after their own deadline if a stuck RPC prevents exit.

## Example: local development

```
PG_URL=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable
MEMCACHED_ADDR=localhost:11211
GRPC_LISTEN=:9090
AUTO_MIGRATE=true
ATL_MIRROR_SCHEMA=true
ATL_ALLOW_APPLY_MUTATION=true
LOG_LEVEL=debug
```

## Example: production

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
