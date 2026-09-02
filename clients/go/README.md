# atlantis-go

Runtime library for [atlantis](https://github.com/rachitkumar205/atlantis) callers: the gRPC transport, the job worker runtime, and the admin JSON helpers. The server lives in the parent directory and is licensed separately.

**This module is not where your typed client comes from.** Entity types and the per-namespace gRPC clients are generated into your own repository by `tide generate`, scoped to the namespaces you consume, and committed there. See [schema flow](../../docs/architecture/schema-flow.md#caller-local-sdk-generation).

## Install

```
go get github.com/rachitkumar205/atlantis/clients/go
```

The module is not yet published to `proxy.golang.org`. Until it is, depend on it via a `replace` directive pointing at a local checkout:

```
replace github.com/rachitkumar205/atlantis/clients/go => ../atlantis/clients/go
```

A versioned release is on the roadmap. Only callers running job workers need this module at all; a caller that just reads and writes entities needs nothing beyond the generated code and `grpc`.

## Dependencies

Compile-time: `google.golang.org/grpc` and `google.golang.org/protobuf`. The SDK does not import any atlantis-server packages — atlantis's `internal/` packages are structurally unreachable from any consumer of `atlantis-go` (see the comment in the parent `go.mod`).

Runtime: an endpoint that speaks the atlantis gRPC protocol — typically a deployed `atlantis` server.

## Regeneration

Callers run `tide generate`, which reads the canonical schema from the server and writes proto sources, wire types and typed clients into the caller's own module. Nothing in this directory is involved.

The `pb/` and `client/` subtrees here are gitignored and exist only for atlantis's own integration tests, built by `make codegen` from whatever `.atl` files are present. Versioned files in `clients/go/`: `go.mod`, `LICENSE`, this README, and the hand-written packages beside them.

## License

[Apache License 2.0](LICENSE).

The atlantis server is licensed separately under [BSL 1.1](../../LICENSE). Production use is permitted except offering atlantis on a hosted or embedded basis in competition with the licensor's paid versions. Importing this SDK into a commercial application is unrestricted; only the server deployment is subject to BSL.
