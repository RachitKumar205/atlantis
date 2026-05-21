# Changelog

All notable changes to Atlantis are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-05-21

Initial release.

### Added

- `.atl` DSL for declaring entities, custom queries, procedures, and TimescaleDB hypertables.
- `tide` caller CLI with `apply`, `plan`, `pull`, `list`, `show`, and `version` subcommands.
- `tidectl` admin CLI with `codegen`, `plan`, `approve`, `lint`, `migrate-up`, `migrate-down`, and `version` subcommands.
- Typed gRPC `Get` / `Create` / `Update` / `Delete` / `Query` methods generated per entity.
- Typed gRPC methods per `query` and `procedure` block declared in `.atl`.
- Read-through cache with automatic invalidation through a transactional outbox.
- pgvector with HNSW indexes for vector search.
- TimescaleDB hypertable support via `partition_field` and `chunk_time_interval`.
- Dev `tide apply` flow that mirrors caller `.atl` files to the server's disk; production workspace-manifest flow that derives schema state from pinned caller refs.
- mTLS between caller and server.

## Known issues (v0.1)

These are not roadmap line items; they are gaps in the current release that callers and operators should be aware of:

- `tidectl codegen` defaults `--schema-dir` to `testdata/schema/`. Operators should pass `--workspace` or `--schema-dir` explicitly; the default will be removed in a v0.1.x patch.

- `tide apply` does not yet regenerate the typed Go client into the caller's `output_dir`. The server prints a hint; in v0.1 the caller runs `buf generate` manually.
- Backfill SQL passed via `tide apply --backfill <file>` is accepted but the server does not yet splice it into the migration. Apply backfills manually before re-running `tide apply`.
- No published server container image; local install requires cloning the repo and building from source.
- No HTTP `/healthz` / `/readyz` endpoints. The standard gRPC Health Checking service is implemented; use `grpc_health_probe`.
- Single-server deployment only.
- Go is the only SDK language.
- PostgreSQL is the only supported database.

[Unreleased]: https://github.com/rachitkumar205/atlantis/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/rachitkumar205/atlantis/releases/tag/v0.1.0
