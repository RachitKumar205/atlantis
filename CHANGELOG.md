# Changelog

Notable changes to atlantis, newest first.

This file exists mainly for **breaking changes**. atlantis generates the DDL
that migrates production databases, so a change to how something is named or
enforced can alter what a later migration does to data that already exists. Those
are called out explicitly, with what to check and what to do about it.

Unreleased entries describe work on `main` that has not been tagged.

## Unreleased

### Changed

#### Each organisation has its own atlantis, reached with its own credentials — breaking

The console dialled one `ATL_ENDPOINT` with one client certificate and sent
every organisation's request down it. That is not a configuration to tighten —
it is a console that cannot serve two organisations at all, and the failure if
it tried would be silent: pointed at the wrong stack it returns the wrong
organisation's schema, plans and jobs, with no error anywhere, because the
connection is perfectly healthy.

**Nothing further along would have caught it.** Traced through every control on
the atlantis side, and a cross-organisation console certificate passes all of
them: both consoles connect as `CN=atlantis-console`, migration `0019` seeds
that caller with `CAPABILITY_OPERATOR` in every install, and
`ATL_CERT_BINDING_EXEMPT_CALLERS` names it by default.

So the address and the certificate became columns. `console.orgs` gains
`atl_endpoint`, `atl_health_addr`, `ca_pem`, `client_cert_pem` and
`client_key_ct` (migration `0005`), and the console builds one channel per
organisation from one row — which is what makes a mismatched pair impossible to
assemble. **Each organisation has its own certificate authority**, so a
mismatched pair that is assembled some other way is refused inside the TLS
handshake, before any atlantis code runs.

**Breaking.** The console no longer reads `ATL_ENDPOINT`, `ATL_TLS_CERT`,
`ATL_TLS_KEY`, `ATL_TLS_CA` or `ATL_HEALTH_LISTEN`. It requires
`CONSOLE_DATA_KEY`, and refuses to start without it. Register each organisation
before its users sign in:

```bash
cloud data-key                      # once, into your secret store
cloud org register -org acme \
  -endpoint atlantis.acme.internal:9090 \
  -health   atlantis.acme.internal:8081 \
  -ca acme-ca.pem -cert console-for-acme.crt -key console-for-acme.key
```

There is deliberately **no fallback endpoint**. An unregistered organisation
gets a 503 naming it. A fallback is the exact failure this change exists to
prevent — one missing row would route an unprovisioned organisation into
somebody else's atlantis and every page would render.

*(`ATL_ENDPOINT` and the TLS trio are unchanged for `tide` and `tidectl`. Only
the console stopped reading them.)*

**`CONSOLE_DATA_KEY` is not recoverable.** Each organisation's private key is
encrypted under it — Tink AEAD, with the organisation name as associated data,
so a ciphertext lifted onto another organisation's row will not decrypt. Lose
the keyset and those rows stay intact, complete, and permanently unopenable,
which presents as every organisation being unreachable with nothing in the
schema looking wrong. Rotation is a keyset property rather than a data
migration: add a key, promote it, and the key id already in each ciphertext
prefix keeps the old ones readable.

This protects a leaked backup, a replica, or a broad read of `console.orgs`. It
does **not** protect a compromised console process, which holds the keyset in
memory by necessity.

One related fix: a cached channel used to outlive *any* failed re-read of its
row, including a deleted one — so de-provisioning an organisation appeared to do
nothing until the console restarted. A definitive answer ("no such
organisation") now evicts and closes the channel; only a failure to reach the
database keeps it, which is what the five-minute refresh was meant to bound.

#### The organisation is now a database boundary in the console

One console process serves several organisations, and until now nothing
separated their rows. Two queries made that concrete rather than theoretical:
the Activity page read `console.audit_log` with no filter of any kind, and
**Sign out all** was `DELETE FROM console.sessions` with no `WHERE` — an admin
of one organisation signing out every organisation on the stack, from a button
labelled as affecting their own.

`console.audit_log` gains an `org` column and a RESTRICTIVE row-level-security
policy reading `console.current_org()`, bound per request from the verified
assertion. Migration `0004` runs automatically.

**What to check.** Nothing, if you have one organisation. Audit rows written
before this migration have no recoverable organisation — `console.users` was
dropped by `0003` and its sessions deleted — so they are parked at `org = ''`,
a value `console.current_org()` never returns. They stay in the table and are
readable by an operator with direct database access, but appear in no
organisation's console. The migration prints how many and how to attribute
them:

```sql
UPDATE console.audit_log SET org = '<your-org>' WHERE org = '';
```

**Two findings worth recording**, both measured on PostgreSQL 17 rather than
assumed:

- **A partition inherits none of its parent's row-level security.** Not the
  switches, not the policies. Reading a child of `console.audit_log` *directly*
  returned every organisation's rows, bound or unbound, while the parent
  behaved correctly. Partitions are now created with RLS enabled and forced and
  no policy of their own, which is deny-all on direct access — nothing reads
  them directly, so that is the right answer.
- **Omitting `WITH CHECK` does not open a write hole.** PostgreSQL reuses
  `USING` as the write check when `WITH CHECK` is absent, and a cross-boundary
  INSERT is refused either way. A comment in `internal/codegen/sql.go` claimed
  otherwise and has been corrected; the shape that genuinely leaks is an
  explicit `WITH CHECK (true)`.

The console refuses to start if any table in its schema is neither policed nor
on a short list of deliberate exemptions (`sessions`, which is the table the
session lookup uses to *discover* the organisation; `spent_assertions`, where a
per-organisation replay check would not be a replay check; and the `orgs`
registry). A missing boundary is otherwise invisible — sign-in works, pages
render, and the data is simply everyone's.

#### The console's identity comes from Atlantis Cloud — breaking

The console no longer holds accounts. `console.users` is dropped, along with
every password hash in it, the sign-in form, the first-run setup wizard, and the
four `/api/users` routes. A user signs in at Atlantis Cloud and arrives carrying
a signed assertion, which the console exchanges for the session cookie it
already used.

**What to do.** Three new settings are required and none has a default —
`CLOUD_ISSUER`, `CLOUD_AUDIENCE` and `CLOUD_JWKS_URL`. The console will not
start without all three. See [Configuration](docs/reference/configuration.md#console-bff).

Strictness here is deliberate rather than fussy. The issuer and audience are
compared for exact equality, and an empty expected value means the check does
not run — so a console missing either would not fail, it would start and accept
assertions from any issuer, for any console. Every request afterwards would
carry a valid session established from a token that verified, and nothing
downstream could tell the difference. There is no later point at which that is
detectable, so it is refused at startup.

**Existing installs.** Migration `0003` runs automatically.

- Every session is deleted. A session row meant "this cookie belongs to local
  user N", and there is no mapping from a local row to a Cloud user — the
  console never knew one. Everybody signs in again, once.
- Audit history is kept and stays attributable. Each row's actor becomes
  `local:<id>`, with the email copied off `console.users` before that table is
  dropped. The `local:` prefix is there so nobody later mistakes one for an
  identity Cloud can resolve.
- `console.users` is dropped. **The password hashes in it are not recoverable**,
  and the down migration cannot restore them — it restores the table's shape
  only. Rolling back a console that has been serving traffic means restoring
  from a backup taken before `0003` ran.

**Step-up changed.** `POST /api/auth/sudo` previously took a password. It now
takes a *fresh* assertion, which sends the user back to Cloud. Assertions are
single use, so the one spent signing in will not work — re-proving identity is
what the control is for, and a replayable credential would have quietly turned
it into a button that always succeeds.

**Audit rows changed shape** for anything reading `console.audit_log` directly:
`user_id BIGINT` is replaced by `actor TEXT` and `actor_email TEXT`, and
`GET /api/audit` renames the corresponding fields. The email is written onto the
row rather than resolved at read time, so an entry says who acted when it
happened rather than who holds that identity now.

#### The console refuses a database role that bypasses row-level security — breaking

`CONSOLE_PG_URL` must name a `NOSUPERUSER`, `NOBYPASSRLS` role. The console
checks at startup and exits otherwise.

The console's tables are about to carry per-organisation data separated by a
RESTRICTIVE row-level-security policy. A policy is only a boundary for a role
the database applies it to: a superuser, or any role holding `BYPASSRLS`, reads
straight through it. The policy is still attached, `\d` still lists it, and
every query returns every organisation's rows.

This is the same failure `pg.RequireIsolatedRole` refuses for the server,
reached by a different door — `CONSOLE_PG_URL` is a separate setting, so a
deployment can get the server's role right and the console's wrong while every
check reports healthy.

Unlike the server's equivalent there is no opt-out flag. A guard that has to be
switched on is a guard that depends on someone remembering, and the moment it
matters is the moment forgetting produces a cross-organisation read.

**Locally:** `make dev-console-role` creates the role and hands it ownership of
the `console` schema — ownership rather than grants, because
`FORCE ROW LEVEL SECURITY` binds a table's owner. It is idempotent, safe on a
database that already has console data, and `make dev-console` runs it for you.

#### Migrations travel in the binary

The server's own schema (`migrations/infra`) and the console's
(`migrations/console`, new) are embedded with `go:embed` and applied from there.
A binary now carries the schema it was built against.

**`MIGRATIONS_DIR` still exists, and means less than it did.** It names only the
**tidectl-emitted** tree — the migrations your deployment owns, written by
`tidectl plan` / `approve` into your own repository after the binary was built,
which no binary can embed. It no longer points at the server's own schema, so it
can no longer point a server at a version of that schema its code disagrees
with. The server image drops its `COPY migrations /app/migrations`.

**The console gains a migration framework.** Its schema was previously built by
one `CREATE TABLE IF NOT EXISTS` block re-run on every boot, with one-shot
`DROP` statements appended as features were removed. That has no version, so
nothing could tell *already applied* from *applied halfway* — survivable while
every statement was `CREATE TABLE`, and not once one is an `ALTER`.

An existing console database converges on first boot with no operator action:
migration 1 is the current schema with `IF NOT EXISTS` throughout, so it no-ops
and records its version. Verified against a database carrying live rows.

The `console.caller_repos` drop from the PR-flow removal is now migration 2,
where its own comment said it belonged.

### Removed

#### The self-host bundle — breaking

atlantis is a managed cloud product and is no longer shipped as a bundle to run
yourself. Gone:

- `docker-compose.self-host.yml` and `deploy/.env.example`
- `deploy/atlantis.service` (systemd unit) and `deploy/pg-init.sql`
- `deploy/reverse-proxy/` (nginx, Caddy and Envoy sample configs)
- `make self-host-up`, `self-host-down`, `self-host-logs`,
  `self-host-caller-cert`, `deploy`, `systemd-install`, `logs`
- `docs/guides/run-behind-a-reverse-proxy.md`

**What did not go.** `deploy/init-certs.sh` stays — `make dev-certs` uses it to
write local mTLS material. `cmd/signer` stays too: it is the only thing that
issues caller certificates, which a managed atlantis needs more than a
self-hosted one did. It belongs with provisioning rather than in the product
repo, and is marked accordingly until that move.

Trusted front-proxy mode (`ATL_TRUSTED_PROXY_CALLERS`) is unaffected — the
feature is a server capability, and it is now documented in [the configuration
reference](docs/reference/configuration.md) rather than in a guide whose
substance was three sample proxy configs.

Local development is unchanged: `make dev-certs`, `make dev-server` (or
`make dev`), `make dev-console`.

### Changed

#### mTLS is required everywhere — breaking

**The server refuses to start without `TLS_CERT_FILE`, `TLS_KEY_FILE` and
`TLS_CA_FILE`.** The console refuses to start without `ATL_TLS_CERT`,
`ATL_TLS_KEY` and `ATL_TLS_CA`. `tide`, `tidectl` and the Go SDK refuse to dial
without a client certificate. Every insecure transport path is gone.

This reads as a security-hardening change and is really an authorization one.
`cfg.TLSCertFile != ""` was the switch behind three separate controls in
`cmd/server/main.go`: the caller allowlist, the caller-to-cert binding, and
admin capability enforcement. atlantis identifies a caller by the CN on its
client certificate, so with no certificate the caller name came from an
`x-caller` header the client writes itself — which the code already described as
"an appearance of authorization rather than authorization".

So the mode that existed was not a less-encrypted atlantis. It was one running
with authentication and authorization off, and it was the mode a developer
reached by default. Every authorization behaviour was unreachable locally,
including whichever one was being worked on.

The specific defect that surfaced it: the console's first-run setup wizard could
not be completed. Its connectivity step reported `overall: "err"` whenever
`ATL_TLS_CERT` was unset — honestly, since it was reporting a real gap — and
"Finish setup" is disabled unless that value is `ok`. Following the documented
TLS-free local setup led to a wizard with no exit.

**To upgrade.** Deployments already running mTLS are unaffected. Anything else
needs certificates before it will start:

```bash
make dev-certs                      # local CA + server and console certs in ./certs
make dev-caller-cert CALLER=backend # a client cert for tide
```

`docker compose --profile isolated up` generates its own into a volume.

`deploy/init-certs.sh` is now incremental rather than all-or-nothing. It leaves
current files alone, reissues one that is missing or expired without disturbing
the CA, and reissues the server certificate when `ATLANTIS_DOMAIN` names a host
its SAN does not cover. Previously a five-of-six state regenerated the CA —
orphaning every caller certificate already issued — and a six-of-six state
skipped even when the server certificate no longer matched the domain.

**Not covered:** `jobs/remote.go` dials caller-hosted job workers in plaintext
and has no TLS option at all. That is a missing capability rather than a
fallback to remove, and it is tracked separately.

### Added

#### Expiry works on tenant-isolated tables

An entity with both `partition by` and `ttl_field` never expired anything. The
sweeper runs on a schedule with no request behind it, so it binds no tenant;
row-level security still applied to its `DELETE`, which matched nothing and
succeeded. Expired rows accumulated forever.

**Declare the entity a `hypertable` on its TTL column and expiry now drops whole
chunks.** Dropping a chunk is DDL, and row-level security filters queries rather
than `DROP TABLE`, so this needs no tenant bound, no registry of tenants to
enumerate, and no database role exempt from the policy — the three routes that
each collided with a decision already made. It is also one operation per chunk
instead of one per row.

`ttl_field` must name the same column the hypertable is declared `on`. Chunks
are selected by the time dimension, so a TTL on a different column could drop a
chunk still holding rows whose TTL has not passed.

A chunk is dropped only once its entire range is past, so a row can outlive its
TTL by up to one `chunk_time_interval`. That interval is the retention precision.

New counter, incremented on every sweep including by zero:

```
atlantis_sweeper_chunks_dropped_total{entity="..."}
```

**`tide apply` now refuses `partition by` + `ttl_field` in any other form.** This
can reject a schema that previously applied. It is deliberate and has no override:
the combination does not work, so accepting it means recording a retention rule
atlantis silently will not honour — and retention is usually a compliance
control, which surfaces as an audit finding rather than a bug report. The error
names all three fixes.

Uses `drop_chunks`, which is Apache-2 licensed. The automated
`add_retention_policy` scheduler is Community/TSL-only and is **not** used —
atlantis schedules the call from its own sweeper, keeping the emitted surface
inside the Apache-2 subset the TimescaleDB pin requires.

### Changed

#### `interval` changes wire type — breaking

**`interval` columns now travel as `atlantis.common.v1.Interval`, not
`google.protobuf.Duration` or a string.**

```proto
message Interval {
  int32 months       = 1;
  int32 days         = 2;
  int64 microseconds = 3;
}
```

Postgres stores those three components separately because converting between
them needs a calendar: `1 month` is 28 to 31 days depending on the month it is
added to, and `1 day` is 23, 24 or 25 hours across a daylight-saving boundary.
`Duration` is seconds and nanos, so it cannot hold a month-bearing interval
without assuming a month length. The reference page previously documented that
assumption — "months are normalized as 30 days" — and a data layer should not
make it. Intervals now round-trip exactly, including months and years.

**This is breaking on paper and not in practice, because `interval` did not
work anywhere before.** Five layers each named a different type: the proto said
`Duration`, the Go type said `time.Duration`, the scan fragment declared a
`string` and assigned it to the proto field, and the dynamic dispatcher
published a plain string. Generated code with an `interval` column did not
compile, so no caller can have been using one through `tide codegen`. A caller
reading such a column through the dynamic dispatcher would have received a
string; that is the only reachable form, and it changes.

### Added

#### Generated server code is compiled

`internal/codegen/compilecheck` holds the emitted server for a fixture schema as
an ordinary package, so `go build ./...` type-checks it. Nothing compiled
generated code before: the emitter tests parse without type-checking, and the
emitted pb import path resolved in no module — it named `atlantis-go`, which is
in neither `go.mod` nor `go.sum`. A signature change on either side of the
emitter boundary was therefore discovered in a caller's build rather than here,
which has happened at least once.

`EmitGoServer` now takes a `GenConfig` like `EmitGoClient`, so the pb and
package paths it writes are configurable; the default is this repository's own
`clients/go/pb`, which is where `buf` actually generates and what `go.mod`
already resolves.

#### Float columns are filterable

`real` and `double` columns can now be used in a `Query<Entity>` filter. They
were already orderable, so a float column could be sorted on and not filtered —
the field was simply absent from the generated filter message, with no error to
say why.

Two new predicate messages, `FloatPredicate` and `DoublePredicate`, carrying the
arms every other numeric type has: `eq`, `neq`, `lt`, `lte`, `gt`, `gte`, `in`,
`is_null`, `is_not_null`.

**Comparisons run at the column's own width.** A `real` column is compared as
float4 rather than widened to float8, and that is load-bearing rather than
tidiness: on a float4 column holding `0.1`, `WHERE score = 0.1::float8` matches
nothing, because the stored value widens to `0.10000000149011612`. Comparing at
the declared width is what makes `eq` on a `real` column find the row.

`eq` on a float is still float equality. A value that was computed rather than
stored from the same literal may not compare equal at either width — the arm
exists because callers who know their data expect it, not because it is safe in
general.

Also fixed alongside: `real` and `double` in the **sandbox**, which mapped them
to the fallback used for types it does not recognise. A float column round-tripped
as opaque bytes and any comparison against it failed in the executor.

#### `varchar` without a length

`varchar` may now be declared with no length limit, matching Postgres. Previously
the parser required `varchar(N)` unconditionally, which left an ordinary legacy
column — `varchar` with no limit — with no `.atl` spelling at all. `text` is not
a substitute: it is a different Postgres type, so declaring `text` against a
`varchar` column reports drift rather than agreement.

`Len 0` was already the "unbounded" sentinel everywhere downstream; only the
grammar could not produce it. Widening `varchar(N)` to `varchar` is additive;
going the other way needs a backfill, as any other narrowing does.

### Added

#### `tide apply` refuses to strand rows with an empty tenant

**New environment variable: `ATLANTIS_ALLOW_UNREACHABLE_TENANT`.** This can refuse
an apply that previously succeeded.

Adding `partition by` to a table whose discriminator column already holds empty
strings makes those rows readable by nobody. `atlantis.set_partition` refuses an
empty tenant — that is what makes an unbound request fail closed — but `''` is a
legal value for a `not null` column, and it is exactly what a legacy column
carries after being added and backfilled with a default. Nothing reported this,
because the policy was behaving precisely as written.

Apply now refuses, naming the table, the column and how many rows are affected,
and telling you to assign them a tenant or delete them. Set the variable to `1`
to apply anyway, accepting that those rows become unreadable.

The check runs only when `partition by` is being added. An already-partitioned
table cannot acquire such a row, because the boundary's `WITH CHECK` refuses the
write.

### Fixed

#### Paging on a nullable column no longer stops short

**Pages that used to end early now continue.** A caller that walked a query to
exhaustion will start receiving rows it never saw.

Ordering by a column that can hold NULL dropped every row from the first NULL
onward. The cursor was a row-value comparison — `("score","id") > ($1,$2)` —
which PostgreSQL evaluates to NULL for any row whose `score` is NULL, so those
rows were filtered out rather than ordered. The page came back empty; empty is
shorter than the limit, so no `next_page_token` was emitted; and the caller,
seeing no token, concluded it had read everything.

Two things had to change together:

- The `ORDER BY` now states `ASC NULLS LAST` / `DESC NULLS FIRST`. These are
  already PostgreSQL's defaults, so no plan and no index choice changes — what
  they buy is that the cursor predicate can rely on the ordering rather than
  assume it.
- A nullable ordering column switches the predicate to an expanded form that
  reaches the NULL group and advances past it. The primary key tiebreaker is
  never nullable, so every page still ends in a strict comparison and the walk
  always terminates.

Page tokens gained a null arm, so a token issued by an older server still
decodes and a token issued by this one is not readable by an older server. Both
are opaque to callers, and a token that fails to decode is rejected as it always
was, so the caller restarts the walk rather than receiving a wrong page.

#### An explicit zero was stored as NULL

**Affects v0.4.0 and every commit up to this one.** **Only the dynamic
dispatcher** — the server that publishes descriptors built at run time. Code
generated by `tide codegen` was never affected, so a caller using generated
clients against generated servers has nothing to check.

Sending `count = 0`, `note = ""` or `active = false` for a nullable column wrote
SQL NULL instead of the value. The row read back as NULL, and nothing reported a
problem at any layer.

The dispatcher builds its protobuf descriptors in Go and marked nullable columns
`proto3_optional`, which by itself does not create presence: presence for a
proto3 scalar lives in a synthetic one-field oneof, which `protoc` writes when it
compiles a `.proto` and which a hand-built descriptor has to supply itself. Every
nullable scalar therefore had no presence at all, and the write path's
"did the caller set this?" check degraded to "does it differ from the zero
value?".

The descriptors now carry the oneof.

**There is no migration, and there cannot be one.** A NULL written by this bug is
byte-identical to a NULL the caller asked for, so nothing can separate them after
the fact. What you can do is bound the exposure: this lists every nullable column
holding at least one NULL, which is the set worth checking against your own
source of truth.

```sql
SELECT c.table_name, c.column_name, c.data_type,
       (xpath('/row/cnt/text()',
              query_to_xml(format('SELECT count(*) AS cnt FROM atlantis.%I WHERE %I IS NULL',
                                  c.table_name, c.column_name),
                           false, true, '')))[1]::text::bigint AS null_rows
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema = 'atlantis'
  AND t.table_type = 'BASE TABLE'
  AND c.is_nullable = 'YES'
ORDER BY 1, 2;
```

Two caveats on reading it. Run it as the table owner or a superuser — under a
role the tenant policy applies to, the counts cover only the bound tenant's rows.
And `null_rows = 0` is the only result that clears a column; any other number is
an upper bound on the damage, not a measurement of it.

#### Widening a tenant column no longer waits for a reviewer

Changing a tenant column from `varchar(16)` to `varchar(32)` was classified
cross-caller-breaking, which requires approval by default. The policy it rebuilt
was byte-identical before and after: both types are text-shaped, so neither takes
a cast and the condition is the same condition. The reviewer was asked to approve
a change to what callers can read, on a change where nothing callers can read had
moved.

The class now follows the policy's **predicate** rather than the column's
rendered type. A change that alters the predicate — `varchar` to `uuid`, where
the discriminator gains a `::uuid` cast — is still cross-caller-breaking. One
that cannot is additive.

The rebuild itself is unaffected and still happens either way: PostgreSQL refuses
to alter a column a policy depends on.

#### A tenant-column rebuild left the table readable across tenants

Changing the type of a column that `partition by` names — widening a
`varchar`, say — requires taking the isolation policy down, because PostgreSQL
refuses to alter a column a policy depends on. The generated migration dropped
the boundary and left the permissive `<table>_default_access USING (true)` grant
standing, so between those two statements the table had row-level security
enabled, nothing restricting it, and something admitting rows: every tenant's
rows to every caller.

`tide apply` runs the whole script in one transaction, and the `ALTER` holds an
ACCESS EXCLUSIVE lock throughout, so no reader ever observed this. It mattered
for a script run by hand, or a runner configured without a transaction, that
died between the two — which left the table cross-tenant readable rather than
merely unavailable.

The migration now raises a `<table>_rebuild_lock` policy — `RESTRICTIVE
USING (false)` — before the boundary comes down, and drops it after the boundary
is back. Restrictive policies AND with every permissive one, so this closes the
table whoever wrote the grants, including an operator who replaced the default
one. If a hand-run now dies mid-rebuild the table denies everything, which is an
outage you can see rather than a leak you cannot.

Nothing about the end state changes, and no action is needed on existing
databases.

#### A `clients/go` worker no longer starts against a server that is too old

`clients/go` is its own Go module, versioned independently of the atlantis
server that applies migrations, so an application can upgrade the SDK past the
release that applied migration 0028. Nothing detected that.

The skew had no early symptom. Claims, leases and completions all worked,
because only the terminal-failure path names `owner_caller`. The first sign was
a job exhausting its retries and wedging: `ReportFailure` failed on the missing
column, the worker logged it at `Warn` and carried on, and the row stayed
`status='running'` with `attempts == max_retries` — which every later claim
excludes. `SweepExhaustedToDLQ`, the safety net for exactly that state, failed
on the same column. The job never reached the dead-letter queue.

`Worker.Run` now checks the catalogue once at start and refuses, naming the
migration and telling you which side to move. Upgrade the server, or pin
`clients/go` back to a release that matches it.

#### Tenant isolation stayed permissive on tables with long names

**Check this if you run `partition by` on a table whose name is 47 characters
or longer.** Migration 0025 moved every tenant boundary from a permissive
policy to a restrictive one. It selected the policies to move by name, matching
`%_tenant_isolation`. atlantis does not always emit that name: a policy
identifier longer than 63 bytes is truncated and given a hash suffix, so on a
long table the policy is called something like
`analytics_customer_engagement_daily_rollup_snapshot_te_6d29d49d`. Migration
0025 did not match it, skipped the table, and reported success.

That matters because 0025 is also what allowed the check that refused any
migration against a table carrying a second permissive policy to be removed.
Permissive policies OR together. So on exactly those tables, one access-control
grant is enough to read every tenant's rows, and nothing reports it — the
declaration is correct, so `tide inspect` agrees.

**Migration 0030 converts what was missed.** It selects boundaries by their
dependency on `atlantis.current_partition()` rather than by name, so truncation
cannot hide one. It carries each predicate verbatim, including hardening you
added yourself, and it does nothing on a database 0025 handled correctly. Run
your migrations to pick it up.

#### `tide apply` put back the access-control grant you replaced

`<table>_default_access` is documented as yours to replace: drop it, write
narrower permissive policies, and tenant isolation is unaffected because it
lives in the restrictive policy. Apply then re-created the total grant. Because
permissive policies OR, that did not add a policy beside your own — it stopped
every one of them constraining anything, while the catalogue still listed them
exactly as you wrote them.

This was not limited to creating an entity. Apply re-emits the policy pair when
`partition by` is added and at the end of any migration that moves the
discriminator column, so a change classified additive was enough to undo an
access-control model.

Apply now creates the default grant only when the table carries no permissive
policy at all, which is the one thing that grant exists to prevent — a table
with only a restrictive boundary admits nothing.

#### `real` and `double` columns

Both types were documented in the type reference and accepted by the parser,
and no mapping behind them was implemented. A schema written exactly as the
reference page described failed at the first step that touched the column:

- `double` rendered as the Postgres type `DOUBLE`, which does not exist, so
  `tide apply` stopped with a syntax error while running the DDL.
- `tide codegen` returned `unsupported type "double" for proto` and generated
  nothing for the whole entity.
- Neither type had a Go mapping, a scan target, or a bind expression, so a
  column that reached generated code scanned into `any` and was discarded.
- The runtime dispatcher published both as protobuf `string`. A client that
  did reach it wrote `""` to a float column and read every value back as 0.
- Ordering a query by such a column produced an unencodable cursor, and the
  emitted handler discarded that error — so the response carried an empty
  `next_page_token`, which the caller reads as "no more rows".

Introspection also reported a `float8` column as `double precision`, the SQL
spelling. It is two tokens, the `.atl` spelling is `double`, and names are
compared as raw strings — so such a column could never match any declaration a
user could write. `tide inspect` reported a permanent mismatch and `tide adopt`
refused to baseline without `--allow-drift`, on a schema that was correct.

All of the above are fixed, and the reference page is now read by the test
suite, so a documented type with no implementation fails the build.

**Upgrading.** A `float8` column in an already-baselined database shows a type
change from `double precision` to `double` on the first plan. The DDL is
`ALTER COLUMN ... TYPE DOUBLE PRECISION`, which is the type the column already
has; Postgres still rewrites the table, so treat it as it is classified rather
than as a no-op. Reaching that state required a schema that could not pass
codegen, so it should not occur in a deployment that was serving traffic.

#### Generated schemas no longer declare types codegen cannot emit

`tide inspect --generate` decided whether it could render a column by checking
whether the type name was a single token. That is not the same question, and
six names passed it that the rest of the toolchain does not implement:
`integer`, `bool`, `timestamp`, `time`, `json` and `inet`. Each is what
introspection returns for an ordinary legacy column.

The failure landed at the worst step. All six render valid Postgres, so plan
and apply ran clean and the checkpoint was written — and `tide codegen` was the
first thing to fail, after the file had been committed and the database
migrated. Those columns are now omitted and named in the file's
`NOT DECLARED` block, alongside `char(n)`.

#### Scheduled jobs now actually run

`atlantis.job_schedules` was written by two functions and read by none: the
scheduler component that `migrations/infra/0006_jobs.up.sql` describes had never
been written. Everything downstream of it was inert —

- `ttl_field` deleted nothing. The TTL sweeper was registered by no call site
  and scheduled by nobody, and its `DELETE ... LIMIT` was MySQL syntax that
  Postgres rejects, so it would have failed on its first entity had it ever run.
  Both are fixed; expired rows are now deleted every five minutes.
- The DSL's `schedule "..."` modifier parsed, validated and produced no fires.

If you relied on `ttl_field`, expect a backlog of expired rows to be deleted
over the first few sweeps after upgrading. The sweeper deletes at most 1000 rows
per entity per fire, so a large backlog drains over several cycles rather than
in one statement.

### Breaking

#### Parked tables are named after their physical table, not the entity

Destructive migrations park objects rather than dropping them: a removed table
is moved into the `atlantis_tombstone` schema and kept for 30 days. The parked
name was derived from the entity's computed `<namespace>_<entity>` name, which
is wrong for any entity that overrides its table with `table "schema.name"` —
and 17 of the 19 schemas in this repository do.

The effect was that the real table was moved into the tombstone schema under
its own name, the rename that should have marked it as parked silently matched
nothing, and the register recorded an object that had never existed. The reaper
then "reaped" the phantom and recorded a successful drop, while the real table
sat unreferenced and unrestorable.

The parked name is now `<source schema>_<source table>__parked` and the register
records the source schema so a restore knows where to put the table back.

**What to check.** If you applied a destructive migration on a build between
`387656d` and this change, look for orphans:

```sql
SELECT c.relname
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'atlantis_tombstone'
   AND c.relname NOT LIKE '%\_\_parked';
```

Anything listed was parked but never registered. Rename it back into its
original schema to restore it; it is not otherwise reachable.

#### Unnamed entity-level CHECK constraints are named differently

A `check "..."` written at entity level without `as <name>` used to be named
positionally:

```
<table>_check_1, <table>_check_2, ...
```

It is now derived from the predicate:

```
<table>_check_<first 4 bytes of sha256(expression)>
```

**Why.** Positional naming renames constraints when they are reordered. Moving
one unnamed check above another changed both names, so the differ had to either
emit `DROP CONSTRAINT` + `ADD CONSTRAINT` — revalidating the whole table under
an `ACCESS EXCLUSIVE` lock for a schema that had not changed — or stay silent and
let a migrated database end up with different constraint names than a freshly
created one. Deriving the name from the predicate makes reordering the no-op it
actually is.

**Who is affected.** Only databases that already contain an unnamed
**entity-level** check. Two common cases are *not* affected:

- `check "..." as my_name` — an explicit name is used verbatim and is untouched.
- A `check` written as a field modifier — named `<table>_<column>_check`, unchanged.

**What goes wrong if you are affected.** Nothing immediately: the migration
diff compares schema to schema, so an unchanged schema produces no DDL. The
problem appears the first time you *remove* or *edit* that check. atlantis emits
`DROP CONSTRAINT IF EXISTS` against the new name, the old name is still what the
database holds, and `IF EXISTS` turns the miss into a silent success. The plan
reports the constraint gone; the database goes on enforcing it.

**How to check.** Grep will not answer this reliably — `.atl` files live in
caller repositories and may be excluded by `.gitignore`, and lowering files one
at a time fails because they cross-reference each other. Parse them all together
and inspect the IR:

```go
// Pass EVERY .atl file to a single Lower() call — they cross-reference each
// other, and lowering them individually fails with "references unknown entity"
// while silently reporting whatever the self-contained files happened to hold.
ir, err := dsl.Lower(files)
for i := range ir.Entities {
    e := &ir.Entities[i]
    for _, c := range e.Checks {   // Entity.Checks, not the cache block
        if c.Name == "" {
            // affected: an unnamed entity-level check
        }
    }
}
```

This repository's own schemas were checked this way: four entity-level checks,
all four explicitly named, none affected.

**What to do if you find one.** Either is fine:

1. **Name it** — `check "..." as the_name_already_in_your_database`. An explicit
   name is used verbatim, so this pins the existing constraint permanently and
   needs no DDL.
2. Drop the old constraint by hand during a maintenance window and let atlantis
   create the new one.

Option 1 is the durable fix, and worth preferring generally: a named constraint
is outside the generated-name scheme entirely.

### Added

- `chunk_time_interval` on hypertables. It was documented three times and
  implemented zero times; it now reaches `create_hypertable`, and changing it
  emits `set_chunk_time_interval` rather than silently doing nothing.
- Cross-entity cache invalidation. `invalidate_on: write(Child where fk =
  self.id)` now invalidates the parent when a child is written, including both
  parents when a child is reparented.
- `Get` is served through the read cache, which was previously built, wired into
  the invalidation worker, and never read from.
- Startup reports which TimescaleDB build the database runs.
  `ATL_REQUIRE_APACHE_TIMESCALE=true` refuses to start on the Community (TSL)
  build.

### Fixed

- CHECK constraints are now diffed at all. Adding one previously produced an
  empty diff and DDL containing no CHECK.
- `partition by` no longer claims tenant isolation it does not provide. It is
  enforced by Postgres row-level security, in the database rather than in each
  generated read.
- No-rows errors from the Postgres adapter are visible to `runtime.IsNoRows`
  again, so a `Get` for a missing row returns NotFound rather than a raw driver
  error.
- Entities written by a custom procedure are no longer served from the read
  cache. A procedure cannot invalidate the row bodies it changes, so caching
  them served stale rows.

### Changed

- **The Postgres image is now the Apache-2.0 TimescaleDB build**
  (`timescale/timescaledb-ha:pg16-oss`) in local dev, the self-host bundle and
  CI. It was `-all`, which is the Community (TSL) edition.

  The Timescale License forbids using TSL software to provide a
  database-as-a-service, and its "Value Added" exception does not cover
  atlantis: that exception requires users be prohibited from modifying the
  database schema via DDL, which is the one thing this product exists to allow.
  Nothing is lost — atlantis uses hypertables only, and `create_hypertable` and
  `set_chunk_time_interval` are both Apache-2.0. `-oss` still carries `vector`
  and `postgis`; the only omission is `timescaledb_toolkit`, which nothing here
  uses.

  Dev and CI match production deliberately: developing against TSL features that
  cannot be shipped is how a licence dependency arrives unnoticed.

  **If you self-host**, running the Community build for your own use remains
  entirely permitted — the licence restricts offering the software as a service,
  not running it. Pin whichever image you prefer.

- Whether a `check` binds to the preceding field or to the entity is now decided
  by indentation rather than by what happens to precede it. A `check` at or left
  of its field's column is an entity-level constraint; indented past it, it is
  that field's. Every schema in this repository parsed identically before and
  after.
- A field may declare only one `check`. A second was previously accepted and
  silently discarded.
