# `tidectl` CLI

```
tidectl <command> [flags]
```

Admin CLI for codegen, migration staging, and applying migrations.

## Environment variables

| Variable | Used by |
|---|---|
| `PG_URL` | `migrate-up`, `migrate-down` |
| `ATL_ENDPOINT` | `adopt` (default for `--endpoint`) |
| `ATL_TLS_CERT`, `ATL_TLS_KEY`, `ATL_TLS_CA` | `adopt` (defaults for `--tls-cert` / `--tls-key` / `--tls-ca`) |

Each variable above is only a default; the corresponding flag overrides it. Other commands read no environment variables.

## Commands

### `tidectl codegen`

Reads `.atl` files, lowers them to the IR, assigns stable proto numbers from the checkpoint, and writes proto/Go/SQL into the output tree.

```
tidectl codegen [--schema-dir <dir>]
                [--workspace <file>] [--workspace-cache <dir>]
                [--out <dir>]
                [--ir-checkpoint <path>]
                [--dry-run]
```

| Flag | Default | Description |
|---|---|---|
| `--schema-dir` | `schema` | Directory scanned recursively for `.atl` files. Ignored when `--workspace` is set. |
| `--workspace` | (empty) | Workspace manifest path. Takes precedence over `--schema-dir`. |
| `--workspace-cache` | `.workspace-cache` | Where the workspace resolver clones caller repos. |
| `--out` | `.` | Output root. Writes `proto/`, `gen/go/server/`, `gen/go/client/`, `gen/go/keys/`, and `gen/.last-ir.json` under this. |
| `--ir-checkpoint` | `gen/.last-ir.json` | Previous IR snapshot used to assign stable proto numbers. |
| `--dry-run` | off | Print what would be written without writing. |

Codegen does not emit migration SQL; use `tidectl plan` for that.

### `tidectl plan`

Diffs the current `.atl` file set against the IR checkpoint and writes a staged migration pair into the stage directory.

```
tidectl plan [--schema-dir <dir>]
             [--ir-checkpoint <path>]
             [--stage-dir <dir>]
             [--migrations-dir <dir>]
             [--destructive]
```

| Flag | Default | Description |
|---|---|---|
| `--schema-dir` | `schema` | Directory of `.atl` files to diff. |
| `--ir-checkpoint` | `gen/.last-ir.json` | Previous IR snapshot for diffing. |
| `--stage-dir` | `migrations/tidectl/_staged` | Where to write `NNNN_tidectl_staged.up.sql` / `.down.sql`. |
| `--migrations-dir` | `migrations/tidectl` | Existing migrations; used to derive the next sequence number. |
| `--destructive` | off | Allow backfill-required or breaking changes. Without this, the plan exits 1 when such changes are present. |

Re-running `plan` overwrites any unapproved staged files.

`plan` stages composite `unique by` edits (adding one is backfill-required and exits 1 without `--destructive`; removing one is additive) and custom query / procedure add, remove, and change (all additive — they're served at runtime and carry no DDL).

### `tidectl adopt`

Verifies the live database matches the declared `.atl` files and seeds the IR checkpoint as the baseline. Reads `atlantis.workspace.yaml` (or `--workspace`), resolves every caller, and batches them into one atomic `AdoptBaseline` RPC — either every caller baselines or none do.

```
tidectl adopt [--workspace <file>] [--workspace-cache <dir>]
              [--endpoint <host:port>]
              [--tls-cert <pem>] [--tls-key <pem>] [--tls-ca <pem>]
              [--allow-drift] [--format {table|json}] [--timeout <duration>]
```

| Flag | Default | Description |
|---|---|---|
| `--workspace` | `atlantis.workspace.yaml` | Workspace manifest path. |
| `--workspace-cache` | `.workspace-cache` | Cache directory for resolved git callers. |
| `--endpoint` | `localhost:9090` (or `$ATL_ENDPOINT`) | Admin gRPC endpoint. |
| `--tls-cert` / `--tls-key` / `--tls-ca` | `$ATL_TLS_CERT` / `$ATL_TLS_KEY` / `$ATL_TLS_CA` | Client cert / key / server CA bundle, PEM. |
| `--allow-drift` | off | Baseline even when introspection finds drift. Records the drift report into `atlantis.adopt_history` for later audit. |
| `--format` | `table` | `table` or `json`. |
| `--timeout` | `120s` | RPC timeout; introspecting a large schema can take a while. |

Exit codes: `0` clean adopt (or drift accepted with `--allow-drift`, checkpoint written); `1` drift detected and the baseline refused; `3` operational error. See [Adopt an existing database](../guides/adopt-an-existing-database.md).

### `tidectl approve`

Moves every staged migration from `--stage-dir` into `--migrations-dir`. Does not re-run codegen or re-diff.

```
tidectl approve [--stage-dir <dir>] [--migrations-dir <dir>]
```

| Flag | Default | Description |
|---|---|---|
| `--stage-dir` | `migrations/tidectl/_staged` | Source directory of staged files. |
| `--migrations-dir` | `migrations/tidectl` | Target directory. |

Exits 1 if `--stage-dir` is empty.

### `tidectl lint`

Parses every `.atl` file in `--schema-dir` and validates the merged schema. Exits 0 if every file parses and the merged schema validates.

```
tidectl lint [--schema-dir <dir>]
```

| Flag | Default | Description |
|---|---|---|
| `--schema-dir` | `schema` | Directory of `.atl` files to check. |

### `tidectl migrate-up`

Shells out to the `golang-migrate` binary to run `migrate up`. Forces the migrations-table parameter to `atlantis_schema_migrations` so the history never collides with another service's `schema_migrations`.

```
tidectl migrate-up [--migrations-dir <dir>] [--pg-url <url>]
```

| Flag | Default | Description |
|---|---|---|
| `--migrations-dir` | `migrations` | Directory passed to `migrate -path`. |
| `--pg-url` | (unset) | Postgres URL. If unset, falls back to `$PG_URL`. Required; the command exits 2 if neither is set. |

Requires the `migrate` binary on `$PATH`.

### `tidectl migrate-down`

Same as `migrate-up` but runs `migrate down 1`. Flags identical to `migrate-up`.

```
tidectl migrate-down [--migrations-dir <dir>] [--pg-url <url>]
```

### `tidectl version`

Prints the build-time version (`tidectl dev` for a local source build; `tidectl v0.4.0` for a release tarball built with `-X main.version=v0.4.0`). Does not contact any service.

```
tidectl version
```

## Exit codes

| Command | 0 | 1 | 2 |
|---|---|---|---|
| `codegen` | success | lower or write failure | arg parse |
| `plan` | success, no changes | destructive change without `--destructive` | arg parse, IO error |
| `approve` | success | nothing staged, or move error | arg parse |
| `lint` | clean | parse or validation failure | arg parse |
| `migrate-up` / `migrate-down` | success | migrate failed | arg parse, missing `$PG_URL` |
| `version` | success | — | — |

## Migration directory layout

The repository ships with two histories on disk:

```
migrations/
├── infra/        # hand-written; runtime machinery (outbox, bookkeeping)
└── tidectl/      # codegen-emitted; caller entities
    └── _staged/  # output of `tidectl plan`; promoted by `tidectl approve`
```

`tidectl migrate-up` targets one directory at a time via `--migrations-dir`. To apply both histories with their separate `_schema_migrations` tables, use the Makefile targets `make migrate-up-infra` and `make migrate-up-tidectl` (or invoke `migrate` directly per directory).

## Output

Every subcommand prefixes its stderr output with its own name (`plan:`, `codegen:`, `lint:`, `approve:`, `migrate:`).
