# Local development

After this recipe you'll have atlantis running on your laptop against a local Postgres, picking up uncommitted `.atl` changes from sibling caller repos.

Prereqs:

- A Postgres instance the operator can reach. A local snapshot of staging or prod is the common case; see [Adopt an existing database](adopt-an-existing-database.md) for the snapshot + restore path.
- A memcached instance on `localhost:11211` (or wherever; the address is configurable). `docker run -d -p 11211:11211 memcached:1.6-alpine` is enough.
- `buf`, `go` (1.25+) and `openssl` on `$PATH`.
- One or more caller repos with `.atl` files. They don't need to be committed.

## 0. Generate certificates

```bash
make dev-certs
```

**atlantis accepts no plaintext connection, locally or anywhere else,** so this comes before everything. The command writes a CA plus server and console certificates into `./certs`, which is gitignored.

Local development is mTLS because the alternative is not "the same thing without encryption". Caller identity is the CN on the client certificate, and the caller allowlist, the caller-to-cert binding and the admin capability grants all read it. A server with no certificate has no identity to authorize, so every authorization behaviour — including whichever one you are working on — is unreachable.

The script is incremental. Re-running it leaves current files alone, reissues a missing or expired one, and reissues the server certificate when `ATLANTIS_DOMAIN` names a host it does not cover.

For a client certificate `tide` can use:

```bash
make dev-caller-cert CALLER=backend
```

It prints the three `TIDE_TLS_*` values to export.

## 0b. Create the console's database role

```bash
make dev-console-role
```

**The console refuses to start on a role that can bypass row-level security**, and the `atlantis` dev role is a superuser. Its per-organisation boundary is an RLS policy, and a superuser reads straight through one — the policy is still attached, `\d` still lists it, and every query returns every organisation's rows.

The target creates `atlantis_console` as `NOSUPERUSER NOBYPASSRLS` and hands it ownership of the `console` schema, which is what `FORCE ROW LEVEL SECURITY` binds against. It is idempotent, and `make dev-console` runs it for you.

## 0c. Run the identity service

```bash
make dev-auth
```

**The console has no local accounts and no development bypass.** It verifies
every sign-in against a JWKS URL in every environment, so running it locally
means running the issuer locally — and that issuer is `cmd/cloud`, the same code
Atlantis Cloud runs, not a test double.

`make dev-auth` creates a signing key in `./certs` on first run and serves the
key set on `:9500`. Leave it running.

To sign in:

```bash
make dev-token EMAIL=you@example.com ROLE=admin
```

It prints a URL with the assertion in the fragment. Open it. The assertion is
**single use** — a second attempt with the same one is refused as a replay, so
run the target again for each sign-in, and again when the console asks you to
confirm a destructive action.

## 0d. Register your organisation

```bash
make dev-org-register ORG=acme
```

**The console has no default endpoint.** One console serves many organisations,
each with its own atlantis behind its own certificate authority, so an address
and a certificate live in `console.orgs` rather than in the environment. Until
an organisation is registered, every page answers 503 naming it.

That is deliberate. A fallback endpoint would mean one missing row silently
routes an unprovisioned organisation into somebody else's atlantis, and every
page would render.

Run this once per organisation you mint tokens for. `ORG` must match the `org`
you pass to `make dev-token`. Order matters: `console.orgs` is created by the
console's own migrations, so start `make dev-console` once first.

Two things about the local setup differ from a deployment, and both are worth
knowing before you read a working `make dev` as proof of anything:

- **One CA, not one per organisation.** There is a single local atlantis, so
  every organisation registers against the same CA and the same certificate. The
  handshake-level refusal of a cross-organisation certificate is therefore *not*
  exercised locally. `internal/console/org_client_pg_test.go` stands up two CAs
  and two servers to exercise it.
- **The keyset is a file.** `make dev-data-key` writes one to
  `./certs/console-data-key` on first use and reuses it thereafter. Deleting it
  does not break the console — it starts fine — but every organisation
  registered under the old keyset stops decrypting, so re-run
  `make dev-org-register`.

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
TLS_CERT_FILE=./certs/server.crt \
TLS_KEY_FILE=./certs/server.key \
TLS_CA_FILE=./certs/ca.crt \
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
- `mTLS is required: TLS_CERT_FILE, ... not set` — run `make dev-certs` and pass the three paths. There is no way to start without them.
- `refusing to start: ... the connecting role bypasses row-level security` from the console — run `make dev-console-role` and point `CONSOLE_PG_URL` at `atlantis_console`.
- `CLOUD_ISSUER is required` from the console — it has no local accounts, so it needs an issuer to trust. `make dev-console` passes the right values; `make dev-auth` runs the issuer they point at.
- `invalid assertion` on sign-in — most often a reused link. Assertions are single use; run `make dev-token` again. Otherwise `make dev-auth` is signing with a different key than the one it publishes: delete `./certs/cloud-signing-key.pem` and restart both.
- `cannot reach the identity provider` — `make dev-auth` is not running. The console answers 503 rather than 401 here, because nothing is known to be wrong with the credential.
- `CONSOLE_DATA_KEY is required` from the console — run `make dev-data-key`, or use `make dev-console`, which passes it.
- `no atlantis is registered for "acme"` — run `make dev-org-register ORG=acme`. The name has to match the `ORG` you minted the token with; signing in creates the organisation but does not provision it.
- `console.orgs does not exist yet` from `cloud org register` — the console creates its own schema at startup. Run `make dev-console` once, then register.
- `decrypt credentials for acme (wrong CONSOLE_DATA_KEY, or the row was tampered with)` — the row was registered under a different keyset than the console is serving with. Most often `./certs/console-data-key` was deleted and regenerated; re-run `make dev-org-register`.
- `no client certificate configured` from `tide` or `tidectl` — run `make dev-caller-cert CALLER=<name>` and export what it prints.
- `pg pool init: ...` from the server — `PG_URL` is wrong or Postgres isn't reachable.
- `memcached: ...` from the server — `MEMCACHED_ADDR` is wrong, or memcached isn't running.
- `permission denied for table ...` — the role in `PG_URL` doesn't have grants on the caller schemas. See [Adopt an existing database](adopt-an-existing-database.md) §3 for the grant SQL.

## Related

- [Adopt an existing database](adopt-an-existing-database.md) — provisioning the local Postgres clone atlantis runs against.
- [Deploy to production](deploy-to-production.md) — the prod-shaped workflow with `source: git` and pinned refs.
- [DSL grammar reference](../reference/dsl-grammar.md) — what goes inside the `.atl` files atlantis reads.
- [Use the sandbox](use-the-sandbox.md) — disposable copies of the merged schema for testing queries against seeded data.
