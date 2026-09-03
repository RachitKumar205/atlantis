# atlantis

This repository builds Atlantis Cloud: the hosted platform, the `tide` CLI,
and the `.atl` schema DSL. One `.atl` declaration produces the Postgres
schema, a typed Go client, a gRPC data plane with per-entity opt-in
caching, and the cache-invalidation logic. Schema changes are planned,
approved, and applied through the platform.

This repository is private: the source is not published, and no outside
contributions are accepted. Release artifacts for the `tide` CLI are
published to the public releases bucket.

## Documentation

- [`docs/`](docs/index.md) — customer documentation: getting started,
  guides, concepts, and the DSL/CLI/API references. Written to
  [`docs/STYLE.md`](docs/STYLE.md).
- [`ops/`](ops/README.md) — operator and internal documentation: server
  configuration, the admin CLI, local development, architecture notes.

## Layout

| Path | Contents |
|---|---|
| `cmd/` | Binaries: the data-plane `server`, the `console` BFF, `cloud` (control plane), `tide` (customer CLI), and internal tools |
| `internal/` | Server, console, and cloud implementation |
| `jobs/` | The job runtime: scheduler, cron, workflow engine, built-in sweeper and reaper |
| `clients/go/` | Hand-written runtime libraries callers link (`jobs`, admin JSON, transport) — `tide generate` writes typed clients into caller repos, not here |
| `web/` | The console and cloud frontends |
| `migrations/` | The platform's own SQL migration histories |
| `atlantis/` | Protobuf definitions for the admin and common APIs |
| `scripts/` | Release packaging, including the `tide` install script |
| `docs/`, `ops/` | Documentation (see above) |

## Development

Local setup, the test matrix, and the codegen loop are covered in
[`ops/local-development.md`](ops/local-development.md). CI runs lint and
tests on every pull request, plus an offline link check over the Markdown
tree when Markdown changes; run lint and tests locally before pushing.

## Naming

Not related to the Terraform tool at
[runatlantis/atlantis](https://github.com/runatlantis/atlantis). The public
brand is Atlantis; the code and prose in this tree spell it `atlantis`.
