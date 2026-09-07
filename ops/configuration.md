# Configuration

Environment variables read by the Atlantis server. The full list lives in `cmd/server/config.go`; `.env.example` ships with defaults that match this page.

## Required

`PG_URL` — PostgreSQL connection string in libpq URL format. The only variable without a default; the server exits at startup if it's unset.

## Reloading and precedence

All variables are read once at startup. Changes require a restart; `SIGHUP` is not handled. Process environment wins over `.env`-file values; no other source is read.

Parse errors are silent: `PG_MAX_CONNS=fifty` runs with the default of `50` and never warns. The startup config dump (info-level log line `config_loaded`) is the only confirmation that your override took effect.

## gRPC listener

| Variable | Default | Notes |
|---|---|---|
| `GRPC_LISTEN` | `:9090` | Address the gRPC server binds to. |
| `TLS_CERT_FILE` | **required** | Server's TLS certificate. |
| `TLS_KEY_FILE` | **required** | Server's TLS key. |
| `TLS_CA_FILE` | **required** | CA certificate for verifying client certs (mTLS). |

**All three are required. The server refuses to start without them,** and there is no mode that accepts a plaintext connection.

This is not only about encryption. atlantis identifies a caller by the CN on its client certificate, and three separate controls read that identity: the caller allowlist, the caller-to-cert binding, and the admin capability grants. A connection with no client certificate has no identity, so none of the three has anything to check. A server without TLS is a server without authorization.

For local development, `make dev-certs` writes a CA and the server and console certificates into `./certs`. `make dev-caller-cert CALLER=<name>` issues a client certificate for `tide`.

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

`PG_QUERY_TIMEOUT_DEFAULT` is the default per-query deadline at the storage layer, enforced via Go context. When the deadline fires, pgx aborts the in-flight query. Raise it for legitimately slow analytical reads; lower it and a runaway query can't pin a pool connection. Per-RPC override is not yet supported.

## Memcached

| Variable | Default | Notes |
|---|---|---|
| `MEMCACHED_ADDR` | `localhost:11211` | Comma-separated for multiple nodes. The `localhost` default fits dev only; production must point at a real memcached, otherwise every read falls through to Postgres with no warning. |
| `MEMCACHED_TIMEOUT` | `100ms` | Per-operation timeout. A node past this deadline causes a cache miss (the read falls through to Postgres) rather than a request error. Raise it to absorb GC pauses on the memcached box. |

## Cache

| Variable | Default | Notes |
|---|---|---|
| `CACHE_LRU_SIZE` | `1024` | Process-wide tier-0 LRU shared across every entity (keys are `entity/id`). Sits in front of memcached. |
| `CACHE_DEFAULT_TTL` | `10m` | Default TTL when an entity's `cache { ... }` block does not declare `ttl=`. |
| `CACHE_XFETCH_BETA` | `1.0` | Probabilistic early-refresh beta. `0` disables (TTL becomes a hard expiry); `1.0` is the published default; `>2` trades cache hits for fewer thundering-herd reloads. Tune only if you see synchronised expiry spikes in the cache-miss histogram. |

## Outbox worker

| Variable | Default | Notes |
|---|---|---|
| `OUTBOX_BATCH_SIZE` | `100` | Rows processed per worker tick. |
| `OUTBOX_DRAIN_INTERVAL` | `250ms` | Time between worker ticks. |
| `OUTBOX_ALERT_LAG` | `5m` | The worker emits a warning log line when the oldest unprocessed row is older than this. |
| `OUTBOX_POINTER_TTL` | `24h` | Memcached TTL on body-cache pointer keys. Must exceed your longest reasonable read latency under load; an expired pointer forces the next reader to refetch from Postgres. |

## Rate limiting

| Variable | Default | Notes |
|---|---|---|
| `RATE_LIMIT_DEFAULT_QPS` | `1000` | Token-bucket refill rate per caller without a `RATE_LIMIT_PER_CALLER` entry. |
| `RATE_LIMIT_BURST` | `200` | Maximum bucket capacity (the largest instantaneous burst allowed before throttling). A caller out of tokens gets `RESOURCE_EXHAUSTED`. |
| `RATE_LIMIT_PER_CALLER` | (unset) | Comma-separated `caller=qps` overrides, keyed by the caller's certificate CN. The console needs a high one — it fans out many reads per page — so a deployment typically sets `atlantis-console=5000`. |
| `RATE_LIMIT_SATURATION_CUTOFF` | `0.80` | Pool-saturation threshold. When pgxpool `AcquiredConns/MaxConns` crosses this, the server returns `RESOURCE_EXHAUSTED` on low-priority RPCs (today hard-coded as method names starting with `List` or `Search`; CRUD and Get never shed). Set to `0` to disable shedding entirely — Postgres then becomes your only backpressure. |

`RATE_LIMIT_PER_CALLER` format: `caller1=qps1,caller2=qps2`. Whitespace around tokens is trimmed. Pairs where the QPS does not parse as a positive integer, or where the `caller=` form is malformed, are silently dropped. Check startup logs to confirm the parsed map.

## Migrations

| Variable | Default | Notes |
|---|---|---|
| `AUTO_MIGRATE` | `false` | Apply pending migrations on boot. |
| `MIGRATIONS_DIR` | `migrations` | Where the **tidectl-emitted** tree lives, resolved relative to the server's working directory. A missing or empty `tidectl/` subdirectory is fine — a deployment with no callers has not emitted any yet. |

`MIGRATIONS_DIR` does not name the server's own schema. That tree is embedded in the binary, so a server cannot be pointed at a different version of the schema its code expects. Only the tidectl tree is read from disk, because `tidectl plan` / `approve` writes it into your deployment repository after the binary was built.

The console has its own embedded tree and its own history table (`console_schema_migrations`); it needs no configuration at all.

Set `AUTO_MIGRATE=false` in production. Boot-time migrations race rolling restarts: golang-migrate serializes on a Postgres advisory lock, but a losing replica crash-loops until the leader finishes — visible to your orchestrator as a flapping pod.

## Admin RPC gating

`ATL_ALLOW_APPLY_MUTATION` selects the schema-change flow. Default (`true`) is the per-caller-CI flow: callers run `tide apply` against the server and the server runs the DDL + IR write under an advisory lock. Set to `false` only when a regulator requires literal SQL review on a deployment-repo PR before any database change (SOX, HIPAA, PCI). The plan and pull RPCs remain available regardless. See [schema flow](architecture/schema-flow.md) for the two flows in full.

`ATL_ALLOW_APPLY_MUTATION` is a switch, not a permission. It decides whether the mutating admin plane is open at all; it says nothing about who may use it.

| Variable | Default | Notes |
|---|---|---|
| `ATL_ALLOW_APPLY_MUTATION` | `true` | Gates the `ApplyMigration` RPC and job/workflow submission for the whole deployment. Set to `false` for the regulated opt-in, or to close the plane during an incident. |

### Who may do what: capabilities

Every admin RPC declares the capability it requires, in `atlantis/admin/v1/admin.proto`. The server reads those declarations from the compiled descriptor at boot and **refuses to start if any method declares none** — an unauthorized endpoint is not something a deployment can be configured into. A single interceptor enforces them, so no RPC depends on its author having remembered a check.

Grants are rows in `atlantis.caller_capabilities`, one per capability:

```sql
-- caller_capabilities.caller references caller_identities, so the caller must
-- be registered first. Registering through the console does both.
INSERT INTO atlantis.caller_identities (caller, can_mutate, created_by)
VALUES ('ci-backend', true, 'you@example.com') ON CONFLICT DO NOTHING;

INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('ci-backend', 'CAPABILITY_SCHEMA_APPLY', 'you@example.com')
ON CONFLICT DO NOTHING;
```

| Capability | Covers |
|---|---|
| `CAPABILITY_SCHEMA_READ` | plan-time reads: merged schema, history, drift |
| `CAPABILITY_SCHEMA_PLAN` | `PlanSchema` |
| `CAPABILITY_SCHEMA_APPLY` | `ApplyMigration`, backfill plans |
| `CAPABILITY_JOBS_READ` / `CAPABILITY_JOBS_WRITE` | job inspection / submission and retry |
| `CAPABILITY_WORKERS_READ` | worker session listing |
| `CAPABILITY_LOGS_READ` | `GetLogs`. Separate from `SCHEMA_READ` on purpose: the log ring carries driver error text, and Postgres renders a unique violation as `DETAIL: Key (email)=(alice@example.com)` |
| `CAPABILITY_OPERATOR` | RPCs whose blast radius lands on other callers: `RollbackSchema`, `AdoptBaseline`, `RegisterCaller`, `RevokeCaller`, `SetCallerAliases`, `DrainWorker`, `EvictWorker` |

There is **no hierarchy**. `SCHEMA_APPLY` does not imply `SCHEMA_READ`, and `OPERATOR` implies nothing at all — a grant confers exactly what it names.

That separation is what lets a PR-time credential compute a migration without being able to run one: grant the plan caller `SCHEMA_PLAN` and nothing else, and the apply caller `SCHEMA_APPLY`. See [Set up CI](https://docs.tryatlantis.dev/guides/set-up-ci/).

Registering a caller through the console grants a bundle derived from its `can_mutate` flag: read capabilities always, plus plan/apply/jobs-write when the flag is set. Clearing the flag revokes those again. `OPERATOR` and `LOGS_READ` are never part of that bundle and survive re-registration, so an operator can grant them by hand without a later registration silently taking them back. The console itself is seeded as an operator by migration `0019_console_identity`.

Two things bound a grant regardless of what it says. A caller may only mutate **its own** namespace — `req.caller` must match the connecting certificate's CN — and operator RPCs refuse to arrive over a trusted front proxy unless `ATL_TRUSTED_PROXY_MAY_OPERATE=true`.

> **Removed.** `ATL_MUTATION_ALLOWED_CALLERS` and `ATL_OPERATOR_ALLOWED_CALLERS` are no longer read, and the server **refuses to start** if either is set. They expressed authorization as process configuration, which meant a grant could not be audited or revoked without a restart — and because the operator list fell back to a global wildcard when unset, which was the shipped default, every caller able to apply schema was also able to roll back the shared checkpoint for everyone.
>
> **Two upgrades need action, including one where you configured nothing.**
>
> Migration `0018_caller_capabilities` converts existing `can_mutate` flags into grants, so `tide plan` and `tide apply` keep working for callers that already had the flag. Anything you had configured through the two removed variables must be granted explicitly.
>
> But the wildcard also meant `tidectl adopt` and `tidectl rollback` worked for *any* certificate whenever `ATL_OPERATOR_ALLOWED_CALLERS` was unset — the default. Those are now `CAPABILITY_OPERATOR`, which 0018 grants only to `atlantis-console`. If you run `tidectl` against this server, grant its CN:
>
> ```sql
> INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
> VALUES ('<your-tidectl-cn>', 'CAPABILITY_OPERATOR', 'you@example.com')
> ON CONFLICT DO NOTHING;
> ```

## Database posture

Two properties of the database atlantis connects to, checked once at startup.
Both are reported either way; the variables decide whether a mismatch is fatal.
Both default to off, because each guards a feature a deployment may not use,
and defaulting to fatal would stop existing installs to protect something they
do not have.

| Variable | Default | Notes |
|---|---|---|
| `ATL_REQUIRE_TENANT_ISOLATION` | `false` | Refuse to start on a deployment where `partition by` would not isolate. Two conditions, both reported either way. **The database role can bypass row-level security** — a superuser, or any role holding `BYPASSRLS`. Such a role sees through `FORCE ROW LEVEL SECURITY`, which leaves every policy attached and completely inert: the catalog looks correct and every read returns every tenant's rows. If the check itself cannot run — a locked-down `pg_roles`, a pooler rewriting `current_user` — the server refuses rather than continuing unchecked. **Or the stored schema contains SQL that can rebind the tenant**: `tide apply` rejects `set_config` and `atlantis.set_partition` in query bodies, procedure steps, `check` expressions and index predicates, but only from the moment that gate existed, so the checkpoint is re-audited at every boot. Set this on any deployment using [`partition by`](https://docs.tryatlantis.dev/reference/dsl-grammar/). |
| `ATL_REQUIRE_APACHE_TIMESCALE` | `false` | Refuse to start on a Community (TSL) TimescaleDB build. Self-hosting on the Community build is legitimate — the Timescale License restricts offering the software as a service, not running it — so this is opt-in and belongs on a hosted deployment. |

## Schema drift

`ATLANTIS_ALLOW_INDEX_DRIFT` controls whether `tide apply` proceeds over an **undeclared unique index** — a live `CREATE UNIQUE INDEX` with no backing constraint, on columns the schema declares but never marks unique. Such an index silently rejects writes the schema considers legal, so apply refuses by default. A partial unique index isn't drift if the schema declares a matching `unique index partial` (same columns; predicate normalized through Postgres to the same expression). A non-partial unique index isn't drift if the columns are declared `unique` / `unique by`. See [Adopt an existing database](https://docs.tryatlantis.dev/guides/adopt-an-existing-database/#legacy-unique-indexes-can-block-apply) for remediation.

| Variable | Default | Notes |
|---|---|---|
| `ATLANTIS_ALLOW_INDEX_DRIFT` | (unset / `0`) | Default refuses the apply with a `DROP INDEX` remediation. Set to exactly `1` to apply anyway with a warning. |

## Unreachable tenant rows

`ATLANTIS_ALLOW_UNREACHABLE_TENANT` controls whether `tide apply` proceeds when adding `partition by` to a table that already holds rows whose discriminator is the **empty string**.

No caller can bind to an empty tenant — `atlantis.set_partition` refuses it, which is what makes an unbound request fail closed — but `''` is still a legal value for a `not null` column, and it is what a legacy column carries after being added and backfilled with a default. Applying isolation over those rows makes them readable by nobody, and nothing reports it, because the policy is behaving exactly as written.

Apply therefore refuses by default, naming the table, the column and how many rows are affected. Assign them a real tenant, or delete them if they are meant to be retired:

```sql
UPDATE billing.invoice SET tenant = '<tenant>' WHERE tenant = '';
```

| Variable | Default | Notes |
|---|---|---|
| `ATLANTIS_ALLOW_UNREACHABLE_TENANT` | (unset / `0`) | Default refuses the apply and reports the row count. Set to exactly `1` to apply anyway, accepting that those rows become unreadable. |

The check runs only when `partition by` is being **added**. An already-partitioned table cannot acquire such a row — the boundary's `WITH CHECK` refuses the write — so there is nothing to scan afterwards.

Note the differences from the `ATL_*` gating variables:

- The prefix is `ATLANTIS_`, not `ATL_`.
- The value is matched against the literal string `1`. Unlike the boolean `ATL_*` vars, `true` / `yes` / `on` do not enable it.
- It only affects `tide apply`. `tide plan` always reports drift as a warning (in `--format=json` output) and never blocks, regardless of this variable.

Drift does not change the plan class or the exit code. See [`tide plan` / `tide apply`](https://docs.tryatlantis.dev/reference/cli-tide/) for how the warning surfaces.

## Trusted front proxy

Lets a TLS-terminating reverse proxy (nginx, Caddy, Envoy) forward a verified client certificate to the server, instead of each client connecting over mTLS directly. The server re-validates the forwarded cert against its own client CA and derives caller identity from it. Inert until `ATL_TRUSTED_PROXY_CALLERS` is set.

SNI/L4 passthrough is the recommended default; reach for this mode only when the proxy must terminate TLS. A plain TLS-terminating proxy without it strips the client certificate and collapses every caller into one identity.

The proxy needs a client certificate of its own whose CN appears in `ATL_TRUSTED_PROXY_CALLERS`, carrying `clientAuth` key usage — that is what the origin's re-validation checks for. The CN must not collide with a real caller. One limit: the leaf must be issued directly off the atlantis CA, because forwarding a chain with intermediates is not handled and fails closed.

| Variable | Default | Notes |
|---|---|---|
| `ATL_TRUSTED_PROXY_CALLERS` | (unset) | Comma-separated cert CNs permitted to assert a forwarded identity. A CN here is treated as a proxy only — its connection must forward a valid client cert. Empty disables the mode. |
| `ATL_TRUSTED_PROXY_CERT_HEADER` | `x-forwarded-client-cert` | Metadata header carrying the forwarded cert (URL-encoded PEM, base64-DER, or an Envoy XFCC value with a `Cert=` element). |
| `ATL_TRUSTED_PROXY_MAY_APPLY` | `true` | Whether a forwarded identity may run its own `tide plan` / `apply`. |
| `ATL_TRUSTED_PROXY_MAY_OPERATE` | `false` | Whether a forwarded identity may invoke cross-caller operator RPCs (register/revoke/adopt/rollback/aliases). High-privilege grant — leave off unless the edge is as trusted as the server. |

The forwarded cert is re-validated (chain to the client CA, `clientAuth` key usage, validity window), and its fingerprint must match the caller's registered binding — so identity, authz, and per-caller cert binding stay enforced at the origin. A direct client forging the header is ignored, because its own peer CN isn't a trusted proxy.

## Schema mirror (dev only)

These variables enable the local-development workflow where the server mirrors caller-submitted `.atl` files to disk so a file watcher can react to schema changes. Both must be `false` in production.

| Variable | Default | Notes |
|---|---|---|
| `ATL_MIRROR_SCHEMA` | `false` | When `true`, the server writes each successful `ApplyMigration` submission to `ATL_MIRROR_DIR`, partitioned by caller. |
| `ATL_MIRROR_DIR` | `schema` | Destination for mirrored caller files. Ignored when `ATL_MIRROR_SCHEMA=false`. |

## Console BFF

Read by `cmd/console`, not the Atlantis server.

| Variable | Default | Notes |
|---|---|---|
| `CONSOLE_LISTEN` | `:3000` | Bind address for the BFF + SPA. |
| `CONSOLE_PG_URL` | (unset; required) | Connection string for the BFF's audit / session tables — the same Postgres instance as the server, separate schema. **The role must be `NOSUPERUSER` and `NOBYPASSRLS`; the console refuses to start otherwise.** |
| `CONSOLE_SESSION_SECRET` | (unset; required, ≥32 chars) | HMAC key for session cookies. Console refuses to start below 32 chars, so `changeme` placeholders trip a fatal startup error — set this before first boot. |
| `CLOUD_ISSUER` | (unset; **required**) | The `iss` value the console will accept, matched exactly. |
| `CLOUD_AUDIENCE` | (unset; **required**) | The `aud` value the console requires, naming this console. It is what stops an assertion minted for one organisation's console being replayed against another's. **Set it to exactly this console's URL as registered with `cloud org register -console-url`** — Cloud mints with that value and redirects to it, so the two must be the same string. |
| `CLOUD_JWKS_URL` | (unset; **required**) | Where the issuer publishes its public keys. Fetched on demand, refreshed every 5 minutes, and refetched whenever an assertion names a key the console does not hold — which is how a key rotation takes effect promptly. |
| `CONSOLE_COOKIE_SECURE` | `false` | Sets the `Secure` flag on session cookies. Default false so `http://localhost` works for first boot; flip to `true` once a TLS terminator (reverse proxy, LB) sits in front. |
| `CONSOLE_AUDIT_RETENTION_DAYS` | `365` | Audit-row retention. Covers the typical SOC 2 audit window and PCI DSS §10.5.1's 12-month online minimum. HIPAA = 2190 (6 years); SOX = 2555 (7 years). `0` keeps every partition forever. |
| `CONSOLE_DATA_KEY` | (unset; **required**) | Base64 Tink keyset. Encrypts the client private key in each organisation's `console.orgs` row. `cloud data-key` prints one; `make dev-data-key` writes one to `./certs` for local use. |
| `ATL_SIGNER_ADDR` | (unset) | The certificate signer's `https://` base URL. Setting it turns on enrolment — see below, and set the rest of the group with it. |
| `ATL_SIGNER_CERT` / `ATL_SIGNER_KEY` | (unset) | The console's own client certificate to the signer. Its common name must be in the signer's `SIGNER_ALLOWED_CLIENT_CNS`. |
| `ATL_SIGNER_CA` | (unset) | Verifies the signer's server certificate. Not the authority any caller is issued from. |
| `CONSOLE_ENROLL_LISTEN` | (unset) | Address for the enrolment listener, e.g. `:3443`. Carries two routes and never the console API or SPA. |
| `CONSOLE_ENROLL_TLS_CERT` / `_KEY` | (unset) | That listener's own server certificate. It terminates its own TLS, unlike `CONSOLE_LISTEN`. |
| `CONSOLE_ENROLL_CLIENT_CA` | **retired — the console refuses to start if it is set** | It named one authority to verify every renewing machine against, which cannot work once each organisation has its own. Renewal now verifies the presented certificate against that organisation's CA from `console.orgs`, after its fingerprint identifies whose it is. Nothing replaces it; unset it. |
| `CONSOLE_ENROLL_PUBLIC_URL` | (unset; **required with the group**) | Where a machine reaches the enrolment listener. Not derivable from the bind address, and deliberately not read from the `Host` header — the page that uses it prints a live token. |
| `SANDBOX_PER_USER_LIMIT` | `100` | Maximum concurrent sandboxes per authenticated user, per organisation. A boot beyond this returns HTTP `429`. The limit also caps fork count — forking N children requires `N + parent` headroom. |
| `SANDBOX_TTL` | `30m` | Idle window after which the BFF's janitor evicts a sandbox. Go duration syntax. Set lower (`10s`) for CI; higher (`2h`) for long agent loops. |

The 256 MiB cap on `PUT /api/sandbox/{id}/snapshot` is a compile-time constant, not configurable. See [Sandbox HTTP API](https://docs.tryatlantis.dev/reference/sandbox-api/#limits).

### Identity comes from Atlantis Cloud

The console holds no accounts. There are no passwords, no user table, and no
first-run wizard: a user signs in at Atlantis Cloud, arrives carrying a signed
assertion, and the console exchanges it for a session cookie.

All three `CLOUD_*` variables are required and none has a default. That is
stricter than it may look, and deliberately so — the issuer and audience are
compared for exact equality, and an empty expected value means the check is
skipped. A console missing either would not refuse to start; it would start and
accept assertions from any issuer, for any console, with every request
afterwards carrying a valid session. There is no later point at which that
could be noticed, so it is refused at startup.

Assertions are single use. Each carries a `jti`, and the console records spent
ones, so a captured assertion is worth nothing once the legitimate request has
landed. The same check backs step-up: `POST /api/auth/sudo` takes a *fresh*
assertion, which sends the user back to Cloud, so holding a session cookie is
not enough to run a destructive action.

A Cloud outage does not sign anyone out. Cached keys keep verifying while the
JWKS endpoint is unreachable, because the assertion is not the durable
credential — it lives for minutes and is spent immediately on a session.
Revoking access means revoking the session, which is a row the console owns and
can delete without reaching Cloud at all.

### Organisations are separated by row-level security

One console serves several organisations. The `org` claim on each assertion is
bound to the database transaction serving the request, and a RESTRICTIVE policy
on `console.audit_log` compares it against every row. A query that forgets to
bind reads nothing rather than everything.

There is nothing to configure. The console **refuses to start** if the boundary
is not in place — enabled, forced, and backed by a policy that actually
references the discriminator. That check is not decoration: binding an
organisation succeeds whether or not a policy exists, and so does every query
afterwards, so an operator signing in sees a working console either way.

Two tables are deliberately exempt, and the console names them rather than
leaving it implied. `console.sessions` is how a request *discovers* which
organisation it belongs to, so it cannot be filtered by that organisation; its
bulk operations are scoped in Go instead. `console.spent_assertions` is global
because a per-organisation replay check would let the same assertion be spent
once in each.

Audit rows written before this existed carry `org = ''` and appear in no
organisation's console. See the CHANGELOG for how to attribute them.

### Each organisation has its own atlantis

The console no longer reads `ATL_ENDPOINT`, `ATL_TLS_CERT`, `ATL_TLS_KEY`,
`ATL_TLS_CA` or `ATL_HEALTH_LISTEN`. One console serves many organisations, each
with its own atlantis behind its own certificate authority, so an address and a
certificate belong to an organisation rather than to the process. They are
columns in `console.orgs`, written by `cloud org register`.

*(The variable names are still live elsewhere. `tide` and `tidectl` read
`ATL_ENDPOINT` for its own connections. Only the console
stopped.)*

```bash
cloud org register \
  -org acme \
  -endpoint atlantis.acme.internal:9090 \
  -health   atlantis.acme.internal:8081 \
  -ca   acme-ca.pem \
  -cert console-for-acme.crt \
  -key  console-for-acme.key
```

Re-running it rotates a certificate in place. A running console notices within
five minutes and rebuilds the channel, so a rotation needs no restart.

**An organisation nobody registered is refused** — HTTP 503, naming the
organisation. There is deliberately no default endpoint to fall back to. A
fallback is exactly the failure this design exists to prevent: one missing row
would route an unprovisioned organisation into somebody else's atlantis, every
page would render, and nothing would report an error.

**A separate certificate authority per organisation is what makes a mistake
loud.** Credentials issued for one organisation do not chain at another's
server, so a mixed-up pairing is refused inside the TLS handshake, before any
atlantis code runs. Nothing further along would catch it: both consoles connect
as `CN=atlantis-console`, that caller is allowlisted in every install, and it
holds `CAPABILITY_OPERATOR`.

**`CONSOLE_DATA_KEY` is not recoverable.** Every organisation's private key is
encrypted under it, with the organisation name as associated data — so a key
lifted onto another organisation's row will not decrypt. Losing the keyset
leaves those rows intact, complete, and permanently unopenable, which presents
as every organisation being unreachable with nothing in the schema looking
wrong. Store it wherever the deployment's other secrets live.

This protects a leaked backup, a replica, or a broad read of `console.orgs`. It
does not protect a compromised console process, which holds the keyset in memory
by necessity.

## Cloud identity service

Read by `cmd/cloud`, which publishes the keys consoles verify against.

| Variable | Default | Notes |
|---|---|---|
| `CLOUD_LISTEN` | `:9500` | Bind address. Serves the JWKS document at `/.well-known/jwks.json`, the sign-in routes, and two probes: `/healthz` answers without touching anything, `/readyz` reaches the database. Probe readiness with `/readyz` — every sign-in Cloud serves is a database call, so `/healthz` and the JWKS route both report success while Postgres is unreachable. |
| `CLOUD_ISSUER` | (unset; **required**) | Becomes the `iss` claim. Must match each console's `CLOUD_ISSUER` exactly. |
| `CLOUD_SIGNING_KEY` | `./certs/cloud-signing-key.pem` | ECDSA P-256 signing key, created on first use with mode `0600`. Persisting it matters: a key regenerated per restart changes the published key set, so every assertion issued beforehand stops verifying. |
| `CLOUD_PG_URL` | (unset; **required**) | Cloud's own database — accounts, organisations, membership, second factors. Separate from the console's; locally the same PostgreSQL instance, schema `cloud`. **The role must be `NOSUPERUSER` / `NOBYPASSRLS` and must own the tables** — see below. |
| `CLOUD_DATA_KEY` | (unset; **required**) | Base64 Tink keyset sealing each account's TOTP secret. `cloud data-key` prints one. Not recoverable if lost. |
| `CLOUD_COOKIE_SECURE` | `false` | `Secure` flag on Cloud's session cookie. Flip to `true` once a TLS terminator sits in front. |
| `CLOUD_PUBLIC_URL` | (unset; **required**) | Base URL every emailed link is built from. See below. |
| `CLOUD_RESEND_API_KEY` | (unset) | Sends through Resend's HTTP API. How a deployment sends. |
| `CLOUD_MAIL_FROM` | (unset; required with a transport) | Sender address, for whichever transport is in use. Resend accepts `Name <addr>`. Supersedes `CLOUD_SMTP_FROM`, which still works. |
| `CLOUD_MAIL_DEV` | `false` | `true` writes messages to the log instead of sending them. Mutually exclusive with a real transport. |
| `CLOUD_SMTP_ADDR` | (unset) | `host:port` of your own mail relay, as an alternative to Resend. |
| `CLOUD_SMTP_FROM` | (unset) | Older name for `CLOUD_MAIL_FROM`. The newer one wins when both are set. |
| `CLOUD_SMTP_USER`, `CLOUD_SMTP_PASSWORD` | (unset) | SMTP credentials. Sent only over an encrypted connection — Go's `PlainAuth` refuses otherwise, which is why STARTTLS is attempted unconditionally. |
| `CLOUD_HIBP_CHECK` | `true` | Refuse passwords found in a known breach, via Have I Been Pwned's k-anonymity range API. The password never leaves the process; only the first five characters of its SHA-1 are sent. |
| `CLOUD_TRUST_PROXY` | `false` | Read `X-Forwarded-For` when rate limiting. Leave off unless something you control terminates in front — the header is spoofable, and a limiter keyed on a spoofable value is one an attacker resets per request. |
| `CLOUD_GITHUB_CLIENT_ID`, `CLOUD_GITHUB_CLIENT_SECRET` | (unset) | Sign in with GitHub. Both or neither — one without the other is refused at startup. Unset means the provider is not registered and its URLs answer 404. |
| `CLOUD_GOOGLE_CLIENT_ID`, `CLOUD_GOOGLE_CLIENT_SECRET` | (unset) | Sign in with Google. Same rule. |

### Signing in takes two factors, always

`cloud serve` publishes the key set, the account routes (sign up, verify,
request a reset, complete one) and sign-in.

**Sign-in is two-legged and cannot be shortened.** A password produces a
*pending login*, not a session; only a second factor turns one into the other.
That is structural rather than a check: pending logins live in
`cloud.pending_logins` and sessions in `cloud.sessions`, so a session lookup
does not find a pending login and there is no flag anybody can forget to test.

**Every account needs a second factor.** An account with none gets a pending
login that reaches enrolment and nothing else, which is also how accounts
created before this existed acquire one. Today that factor is TOTP, with ten
single-use backup codes; **WebAuthn arrives with the Cloud sign-in app**, which
is what can drive its browser ceremony.

**Completing a password reset does not sign you in.** Proving control of a
mailbox is one factor, and a reset that produced a session would make the
mailbox sufficient on its own.

**An unverified address cannot sign in at all**, so there is no
half-authenticated account anywhere downstream. `POST /api/auth/verify/resend`
issues another link and answers identically whether or not the address has one.

### Signing in with GitHub or Google

Set a provider's client id and secret and its routes appear:
`GET /auth/github` starts a sign-in and `GET /auth/github/callback` finishes
one. Register the callback with the provider as `CLOUD_PUBLIC_URL` plus
`/auth/github/callback`. Cloud builds that value from `CLOUD_PUBLIC_URL` and
never from the request, because a redirect target taken from a `Host` header
is one an attacker chooses.

**A provider is one factor, not two.** The callback produces the same pending
login a password does, and the same second factor is still required — a
provider vouching for somebody is evidence about an address, not proof of
possession of anything Cloud issued. Nothing about the section above bends for
OAuth.

**Only an address the provider says is verified is accepted.** GitHub's must be
flagged both primary and verified; Google's needs `email_verified`. An account
with no such address is refused rather than asked to type one, because
`cloud.users.email` is unique and an unproven address is a way to collide with
somebody who proved theirs.

**A provider address matching an existing account connects the two, but only
when that account already has a second factor.** With one, the provider's word
gets somebody as far as a challenge they cannot answer — exactly where a stolen
password gets them. With none, there would be nothing between that word and the
account, so the sign-in is refused and nothing is written; the person signs in
with their password instead and connects the provider afterwards.

**The last way in cannot be removed.** `GET /api/account/identities` lists
connections and `POST /api/account/identities/{provider}/unlink` removes one,
except when it is the only thing that can reach the account — an account with no
password and one connection has no recovery path, because resetting a password
requires having one. Connections remain listed and removable after a provider's
credentials are taken out of the environment.

### Getting into an organisation's console

`GET /authorize?org=<name>` is how a signed-in Cloud user reaches a console. It
checks `cloud.memberships`, mints an assertion carrying **that row's role**, and
redirects to the organisation's registered console with the token in the URL
fragment. The console spends it once for a session.

**There is no destination parameter.** The URL comes from
`cloud.orgs.console_url`, set by `cloud org register -console-url`. A request
cannot name where it wants to be sent, so there is no open redirect to guard
against — and a `console=` parameter, if anybody adds one to a link, is ignored.

**That URL is also the assertion's audience.** Set the console's
`CLOUD_AUDIENCE` to exactly the same value; `cloud org register` prints it for
that reason. A difference of one character is every sign-in failing with a
message about the token rather than about the mismatch.

**An organisation with no registered console cannot be signed in to**, and says
so. That is the ordinary state between `cloud org create` and provisioning.

`cloud mint` still exists for the cases with no browser — an operator
diagnosing a deployment, and the first membership in a new one. It now reads
the same membership row and refuses without one, and it has no `-role` flag:
the row decides, so the command and the endpoint cannot disagree.

### Moving between organisations

An account in more than one organisation gets a switcher in the console's
sidebar. Picking an entry is a full-page navigation to `/authorize?org=<name>`
— the same route as above, doing the same three things — so the browser may
come back to this console or land on a different deployment entirely,
whichever that organisation registered. Nothing needs configuring for either.

Assertions carry an `orgs` claim naming every membership. Names only: no
roles, no endpoints. The console keeps it on the session row and serves it from
`/api/auth/me`, because the console cannot ask Cloud what somebody belongs to —
that separation is what keeps a Cloud outage from being a console outage.

**The list is a snapshot; the membership is the gate.** An organisation removed
at Cloud keeps appearing in that person's switcher until their session ends,
and choosing it gets a refusal page, because `/authorize` re-reads the row
before minting. Nothing in a console decides anything from this claim.

**Every console a user reaches learns the names of their other
organisations.** That is the cost of a switcher that can offer an organisation
living on another deployment, and it is accepted deliberately.

With one organisation the sidebar shows a label and no control.

### Giving a machine a certificate

An admin mints a single-use enrolment token from the Callers page. The machine
that will hold the certificate generates its own key, builds a certificate
signing request, and presents the token and the CSR to the enrolment listener.
**No private key crosses the wire in either direction.**

The console previously generated the key itself and offered it for download.
That path required `ATL_SIGNER_ADDR`, which no deployment set, so it answered
503 everywhere it ran.

**Enrolment is all-or-nothing.** A console with some of the group set refuses to
start and names what is missing. A console with none of it set runs normally
with enrolment off, and the mint route says so rather than answering 404.

**The token is single-use and short-lived.** Unused, unexpired and belonging to
the right organisation are all conditions on the one statement that spends it,
so an expired token stops working immediately rather than when housekeeping next
runs. The console stores only its SHA-256; it cannot be read back.

**Enrolling supersedes.** atlantis binds a caller to one certificate, so
completing an enrolment stops the previous one authenticating — for every
machine still using it. The console warns before you start, and cannot tell you
whether there is a previous one: it does not see atlantis's fingerprint, only
its own record of what it has enrolled.

#### Renewing

A machine renews by presenting the certificate it is replacing — no token. The
request carries only a CSR: the organisation and the caller come from the
console's record of what it issued, looked up by the presented certificate's
fingerprint. A certificate this console did not issue cannot be renewed here,
which includes anything from `make dev-caller-cert`; those keep authenticating
and are replaced by enrolling.

**A replaced certificate keeps working until it expires.** atlantis no longer
binds a caller to one certificate, so a renewal whose response is lost — a
timeout, a 502, a crash before the file lands — leaves the machine holding one
that still works, and it simply tries again. This needed a 24-hour overlap
window while certificates were pinned; shortening them to seven days removed
both the pinning and the need for the window.

**`tide` renews on its own**, at two thirds of the certificate's life, which is
around day five of seven. A failed renewal is a warning rather than an error:
the certificate is not expired yet, and refusing to run because a refresh failed
would turn a console outage into a caller outage.

**Ephemeral CI runners enrol per job** rather than holding anything between
runs: `tide login --oidc` trades the runner's workload identity for a
one-hour certificate under a federation rule configured on the console's
Callers page. The variables that used to carry a private key into CI are gone
and stay gone; see `docs/examples/README.md` for the workflow shape.

#### The signer

| Variable | Default | Description |
|---|---|---|
| `SIGNER_LISTEN` | `:7070` | mTLS, carries `POST /issue`. |
| `SIGNER_HEALTH_LISTEN` | `:7071` | Plaintext, carries `GET /healthz` only. Separate because the container health check holds no certificate. |
| `SIGNER_TLS_CERT` / `_KEY` | (unset; **required**) | The signer's server identity. |
| `SIGNER_CLIENT_CA` | (unset; **required**) | Who may ask for a certificate. |
| `SIGNER_ALLOWED_CLIENT_CNS` | (unset; **required**) | Comma-separated common names, normally `atlantis-console`. |
| `CA_DIR` | `/ca-private` | The issuing authority. Never leaves the signer. |
| `PG_URL` | (unset; **required**) | Reads `atlantis.caller_identities` so issuance is gated on a registered caller. |

**`SIGNER_CLIENT_CA` must not be the authority in `CA_DIR`.** Every caller
certificate the signer issues is marked for client authentication, so a signer
trusting its own issuing authority would accept every certificate it had ever
produced as a credential — and one caller could then obtain another's identity.
The common-name allowlist is a second, independent answer to the same question.

`PG_URL` is required rather than optional. It used to be skipped entirely when
unset, which meant the deployment with the least configuration had the fewest
checks.

#### The provisioner

Turns queued organisations into running ones. It claims from the provisioning
queue, builds the organisation in Kubernetes, registers it with the console, and
points Cloud at that console — so that creating an organisation is the only
step a person takes.

It runs as its own process rather than inside `cloud serve`, because it holds
Kubernetes credentials and `cloud serve` holds every password, every second
factor and the assertion signing key.

| Variable | Default | Description |
|---|---|---|
| `CLOUD_PG_URL` | (unset; **required**) | The queue, the organisations, and the audit log. |
| `CONSOLE_PG_URL` | (unset; **required**) | Where a provisioned organisation is registered. |
| `CONSOLE_DATA_KEY` | (unset; **required**) | The keyset that seals each organisation's private key. Must be the one the console serves with. |
| `CLOUD_AUDIENCE` | (unset; **required**) | The console's URL. Written to `cloud.orgs.console_url`, and must be an absolute `http(s)` URL. |
| `PROVISIONER_EXTERNAL_HOST` | (unset; one of the two is **required**) | The one name every organisation is reached at; the port tells them apart. A name, never an address — it goes in every certificate's SAN. |
| `PROVISIONER_ORG_DOMAIN` | (unset; one of the two is **required**) | Gives each organisation its own name, `<org>.<domain>`, in its certificates and endpoints. A wildcard DNS record under the domain points them all at the same load balancer. Setting both this and `PROVISIONER_EXTERNAL_HOST` is refused. Cloud reserves the platform's own labels (`platform`, `console`, `enroll`, `docs`, `releases`) as organisation names. |
| `PROVISIONER_SERVER_IMAGE` | (unset; **required**) | The atlantis image. |
| `PROVISIONER_SIGNER_IMAGE` | (unset; **required**) | The signer image. |
| `PROVISIONER_POSTGRES_IMAGE` | (unset; **required**) | The Postgres image. Its tag must read as a Postgres version — see `Dockerfile.pg`. |
| `PROVISIONER_ENROLL_URL` | (unset) | The enrolment listener's public address, written into `cloud.orgs.enroll_url` per organisation so `tide login` needs no configuration. An organisation registered without one refuses the browser login at the poll, naming what is missing. |
| `PROVISIONER_MEMCACHED_ADDR` | (unset; **required**) | The shared cache. Not defaulted deliberately; see below. |
| `PROVISIONER_HEALTH_LISTEN` | `:8082` | Plaintext `/healthz` and `/readyz`. Binds every interface, because the orchestrator probes it. |
| `PROVISIONER_METRICS_LISTEN` | `127.0.0.1:9102` | Plaintext `/metrics`, on its own listener. Loopback by default: it shared the health port until the per-organisation counts turned out to be readable by any pod in the cluster. Move it to a reachable address when something scrapes it, and put a credential in front at the same time. |
| `PROVISIONER_READY_TIMEOUT` | `5m` | How long to wait for an organisation to serve. Exceeding it is retryable, not terminal. |
| `PROVISIONER_LEASE` | 3 × ready timeout | How long a claim is held. Must exceed the ready timeout. |
| `PROVISIONER_LEASE_HEARTBEAT` | `30s` | How often the lease is extended during a wait. |
| `PROVISIONER_POLL_INTERVAL` | `10s` | How often an idle queue is checked. |
| `PROVISIONER_RECONCILE_INTERVAL` | `5m` | How often ready organisations are checked against the cluster and requeued if absent. |
| `PROVISIONER_CONSOLE_CERT_RENEW_WITHIN` | `240h` (10 days) | How much life the console's certificate for an organisation must have left before a reconcile pass replaces it. Paired with the 30-day certificate lifetime, so a credential is replaced with a third of its life to spare. |
| `PROVISIONER_RETRY_BASE` / `_MAX` | `30s` / `30m` | Backoff after a failed attempt: doubling, capped. |
| `PROVISIONER_NAME` | the hostname | Names this process in the queue. In Kubernetes the hostname is the pod name. |

Kubernetes credentials are not a setting. The provisioner uses the pod's service
account in-cluster and the ambient `KUBECONFIG` otherwise, so the same binary
works in both places.

**`PROVISIONER_LEASE` must outlive `PROVISIONER_READY_TIMEOUT`, and the process
refuses to start otherwise.** A readiness wait can burn the whole timeout, and a
lease that expires during it lets a second provisioner claim an organisation the
first is still building — which then finishes and writes its result over the
row. The default is computed from the timeout rather than being a constant, so
raising one raises the other.

**`PROVISIONER_MEMCACHED_ADDR` has no default on purpose.** `cmd/server` defaults
`MEMCACHED_ADDR` to `localhost:11211`, which inside a pod is the one value
guaranteed to be wrong — and atlantis's readiness probe performs a real cache
operation, so a wrong address means the organisation never becomes Ready rather
than merely being slow.

Seven further settings — namespace prefix, storage class, pull policy, pod CIDR,
operator namespace, and the Postgres instance count and volume size — are read
but have their defaults inside the provisioning package. Leave them unset unless
you are overriding one deliberately.

**Running more than one is safe.** Claims are taken with `FOR UPDATE SKIP
LOCKED` under a lease, so two provisioners never take the same organisation, and
one that dies mid-work frees its organisation when the lease expires. There is no
sweeper to run.

**It reconciles absence, not shape.** Every `PROVISIONER_RECONCILE_INTERVAL` it
asks the cluster whether each ready organisation still exists, and requeues the
ones that do not. It does **not** detect drift inside a namespace — a Deployment
scaled to zero, a NetworkPolicy removed, a Secret edited — because a partial
version of that would report an organisation as reconciled while leaving whole
classes of drift unchecked.

**The one exception is the console's certificate**, which the same pass replaces
when it is within `PROVISIONER_CONSOLE_CERT_RENEW_WITHIN` of expiring. That is
not drift — nothing changed it — but it needs the same fleet-wide sweep, and
this is the only loop that makes one. Both certificate authorities are kept, so
no caller certificate is affected and nothing in the organisation restarts; the
console picks up the replacement within its refresh interval.

**Watch `atlantis_provisioning_console_cert_seconds_left`.** It counts down to
the soonest-expiring console credential in the fleet and should saw-tooth as
credentials renew. Falling steadily means rotation has stopped, and every way it
can stop looks the same from outside — a wedged provisioner, expired Kubernetes
credentials, an unreachable console database. None of them raise an error anyone
sees, and the first visible symptom without this metric is an organisation
nobody can open in a browser. Alert well above the renewal window; reaching the
window already means a pass was missed. The metric reads `NaN` until the first
pass measures something, so a freshly started provisioner does not trip the rule.

Rebuilding an organisation mints a **new certificate authority**, because the old
one lived in a Secret that went with the namespace. Every caller certificate
issued under it stops working, and those callers must enrol again. Watch
`atlantis_provisioning_reconciled_total`: it should be zero, and a non-zero value
means somebody's namespace disappeared.

**Credentials are re-read, not cached.** If the cluster refuses this process —
a rotated authority, an expired token — it rebuilds the connection and retries
once. If that fails it reports itself unready and stops claiming, rather than
marking healthy organisations failed one per tick with an error that names none
of them.

### Confirming a destructive action

The console's danger-zone actions need step-up, and step-up means presenting a
second factor at Cloud — the console holds no credential of its own to re-check.

`GET /authorize?org=<name>&prompt=reauth` asks for a code even when the session
is live, and only then mints an assertion carrying a `step_up` claim. The
console's `POST /api/auth/sudo` requires that claim.

**Why the claim rather than freshness.** Sudo used to accept any unspent
assertion, which was strong while the only way to get one was an operator with
the signing key. `/authorize` changed that: a Cloud session mints a fresh
assertion on request and lasts twelve hours without a factor being presented.
Single-use stops an assertion being replayed; it does nothing about one being
re-minted. The claim is what says a factor was actually presented.

The console opens that URL in a popup and takes the result through
`postMessage`, so the dialog and the action behind it survive. If the browser
blocks the popup, the dialog falls back to pasting the token by hand.

### Cloud needs its own database role

`CLOUD_PG_URL` must point at a role that is `NOSUPERUSER` and `NOBYPASSRLS`,
**and that owns the tables in schema `cloud`**.

`cloud.totp_secrets` and `cloud.backup_codes` are protected by row-level
security. `FORCE ROW LEVEL SECURITY` binds a table's *owner*; it binds a
superuser to nothing. So a Cloud connected as a superuser — or one holding only
`SELECT`/`INSERT` on tables somebody else owns — runs with every policy attached
and inert, `\d` still listing them, every query returning everything.

Cloud refuses to start in that state: the boot check that has been watching
since the schema was created now enforces, and asks about the role as soon as a
policed table exists. Locally, `make dev-cloud-role` creates the role and
transfers ownership of anything an earlier superuser-run Cloud left behind.

**`CLOUD_DATA_KEY` is not recoverable.** A TOTP secret must be recomputed to be
checked, so unlike the argon2id password hash beside it, it is encrypted rather
than hashed — sealed with the user id as associated data, so a ciphertext lifted
onto another account will not open. Lose the keyset and every enrolled factor
stays intact and permanently unopenable, which presents as every account being
unable to finish signing in.

**`CLOUD_PUBLIC_URL` has no default and is not derived from the request.** It
becomes a URL in an email asking somebody to prove who they are. A wrong value
does not fail — it sends every user a working link to the wrong host, which is a
broken flow if that host is ours and a phishing primitive if it is not. Deriving
it from the `Host` header would be worse, because that header is
attacker-controlled.

**Cloud refuses to start with no mail transport.** Set `CLOUD_RESEND_API_KEY`
with `CLOUD_MAIL_FROM`, or `CLOUD_SMTP_ADDR` with a sender, or
`CLOUD_MAIL_DEV=true` to write links to the log during development.

That refusal replaced a fallback, and the reason is that this was the one
setting whose absence looked exactly like everything working: accounts created,
the response saying a message is on its way, and the link sitting in a log
nobody reads. The logging mailer still warns on every send — it is now something
you ask for rather than something you end up with.

**Resend needs its sending domain verified** before anything will leave. An
unverified domain is a 403 whose message names domain verification rather than a
generic failure, because "email is not arriving" is otherwise a long thing to
diagnose. Resend recommends a subdomain over the root domain, to keep sending
reputation separate.

**Sign-up and reset-request answer identically whether or not the address has an
account** — same status, same body, and held to the same latency floor. Without
the floor the bodies are pointless: the registered path writes a row, mints a
token and sends a message, and the unregistered one does none of it, so the
clock says what the body will not. An address that already has an account is
sent a message telling *the owner* that somebody tried to sign up with it.

`cloud org register` also reads `CONSOLE_PG_URL` and `CONSOLE_DATA_KEY`, as
defaults for its `-db` and `-data-key` flags. The keyset must be the one the
console serves with: registering under a different key writes a row that looks
complete and never opens.

Minting is a command (`cloud mint`) rather than an HTTP route, and that is a
constraint rather than an unfinished feature. A route that mints on request is a
complete authentication bypass until something in front of it establishes who is
asking, and Cloud does not yet hold user records. `cloud mint` needs read access
to the signing key, so it is available to whoever operates Cloud and nobody
else.

`cloud org register` is the same argument one layer along. Registering an
organisation decides which atlantis a console hands that organisation's users,
so an unauthenticated route for it would let anyone who can reach the console
repoint an organisation at a server they control — and every request afterwards
would succeed, because the credentials would be genuine.

## Observability

| Variable | Default | Notes |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP gRPC collector endpoint (e.g. `otel-collector:4317`). Empty disables OTel export; Prometheus metrics on `:8081/metrics` (which needs a client certificate) and structured logs are unaffected. |

## Logging

| Variable | Default | Notes |
|---|---|---|
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |

## Parsing

Booleans accept `1`, `true`, `yes`, `on` (case-insensitive) for true and `0`, `false`, `no`, `off` for false.

All typed variables (int, float, duration, bool) silently fall back to their default on parse error — see [Reloading and precedence](#reloading-and-precedence) above for how to verify your value actually took effect.

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

## Example: production (default flow)

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

## Example: production (regulated opt-in)

For SOX, HIPAA, or PCI workloads that require literal SQL review before any database change:

```
# ...same as above, except:
ATL_ALLOW_APPLY_MUTATION=false
```

To scope mutation to a single CI identity, grant it and no one else:

```sql
INSERT INTO atlantis.caller_capabilities (caller, capability, granted_by)
VALUES ('ci.deploy.internal', 'CAPABILITY_SCHEMA_APPLY', 'you@example.com');
```
