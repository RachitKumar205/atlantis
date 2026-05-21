# Concepts

Four concepts before writing `.atl` files. Start with [Schema as code](schema-as-code.md); the rest reference it.

- [Schema as code](schema-as-code.md). `.atl` syntax and the SQL, gRPC, and clients derived from it.
- [The typed query surface](the-typed-query-surface.md). The `Get`/`Create`/`Update`/`Delete`/`Query` methods generated per entity.
- [Caching and invalidation](caching-and-invalidation.md). Declaring read-through cache in `.atl` and how writes invalidate it.
- [`tide` vs `tidectl`](tide-vs-tidectl.md). Which CLI runs where and why.
