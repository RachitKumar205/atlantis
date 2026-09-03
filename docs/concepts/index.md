# Concepts

- [Schema as code](schema-as-code.md). The `.atl` files in git are
  authoritative; the database derives from them.
- [How atlantis runs your schema](how-atlantis-runs-your-schema.md). The
  checkpoint, the apply, hot reload, migration ownership.
- [The typed query surface](the-typed-query-surface.md). What the generated
  `Get`/`Query`/`Create` methods can express.
- [The generated client](the-generated-client.md). Committed code, stable
  proto numbers, `generate --check`.
- [Schema versioning](schema-versioning.md). The append-only version
  registry: history, blame, staleness.
- [Change approval](change-approval.md). Classes, tiers, rehearsals,
  protections, freezes, overrides.
- [The console](the-console.md). Every page, and what deciding costs.
- [Custom queries and procedures](custom-queries-and-procedures.md). SQL
  beyond the typed surface.
- [Caching and invalidation](caching-and-invalidation.md). The two caches
  and the transactional outbox.
- [Jobs and workflows](jobs-and-workflows.md). Typed background work and
  multi-step orchestration.
- [Ephemeral data](ephemeral-data.md). Typed, expiring stores with no
  table behind them.
- [The sandbox](sandbox.md). Disposable test databases — schema only, no
  production data.
