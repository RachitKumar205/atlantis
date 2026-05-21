# `tidectl` CLI

```
tidectl <command> [flags]
```

Admin CLI for codegen, migration staging, and applying migrations.

## Environment variables

| Variable | Used by |
|---|---|
| `PG_URL` | `migrate-up`, `migrate-down` |

Other commands read no environment variables.

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

Prints `tidectl 0.1.0`. Does not contact any service.

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
