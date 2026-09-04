# The typed query surface

For each entity, atlantis generates a typed gRPC service. The service
exposes primary-key `Get`, `BatchGet`, `Create`, `Update`, and `Delete`
methods, plus a `Query` method that takes a predicate tree and compiles it
to parameterised SQL on the server.

The request and response messages come from the entity's `.atl`
declaration, so a field rename or type change becomes a compile error in
the caller.

## How `Query` works

`Query` filters rows of a single entity; it does not aggregate or join.
The request takes a structured filter expression built from per-entity
predicate messages generated from the schema:

```go
&app.QueryNoteRequest{
    Filter: &app.NoteFilter{
        Title:     &app.StringPredicate{Like: proto.String("%draft%")},
        CreatedAt: &app.TimePredicate{Gte: timestamppb.New(after)},
    },
    OrderBy: &app.NoteOrderBy{CreatedAt: app.SortOrder_DESC},
    Limit:   100,
}
```

Each scalar field type has a corresponding predicate. The full operator
set is in [the DSL grammar reference](../reference/dsl-grammar.md).

## Properties of a predicate tree

Typed predicates give three guarantees:

- Injection safety. The server binds parameters from the predicate; there
  is no string to escape.
- Cache-key stability. The query-result cache canonicalises equivalent
  expressions to one key.
- Rename safety. A renamed field becomes a compile error in the caller's
  generated code.

## When `Query` isn't enough

`Query` is a typed predicate over one entity. Reads that need `GROUP BY`,
`DISTINCT ON`, multi-entity joins, sampling, or window functions do not
fit; declare a `query` or `procedure` block instead. The server validates
the SQL when you run `tide apply`, and the codegen emits a typed RPC.

A brand-new custom query's RPC registers only when the server restarts;
see [adding vs editing](custom-queries-and-procedures.md#adding-vs-editing).
Preview the custom SQL in the [sandbox](sandbox.md) before applying.

## Related

- [Custom queries and procedures](custom-queries-and-procedures.md)
- [Caching and invalidation](caching-and-invalidation.md) — how `Query` results are cached.
- [The DSL grammar](../reference/dsl-grammar.md) — predicate types and operators.
- [The sandbox](sandbox.md) — disposable copies of the schema for testing predicates and custom SQL.
