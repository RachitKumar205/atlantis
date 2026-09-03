# Declare a background job

Declare a typed job in `.atl`, implement its handler in your own service,
and run the submit–claim–complete loop.

## Prerequisites

- A caller set up with `tide` ([Get started](../getting-started/index.md)).
- The `github.com/rachitkumar205/atlantis/clients/go/jobs` package in your
  service, for the worker and handler types.
- Job dispatch enabled for your queue on your organisation's server. It is
  not on by default — contact atlantis support with the queue name before
  the first worker connects.

## 1. Declare the job

In your repository's `.atl`:

```atl
job ImportContacts in directory {
  args {
    account_id      varchar(64) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries  3
  timeout  30m
  queue    "contacts"
  visible_to "directory"
}
```

- `args` uses the entity field grammar (varchar, int, jsonb, arrays).
- `visible_to "directory"` restricts which caller may submit the job and
  claim it.

## 2. Apply

```bash
tide apply
```

The declaration is additive. The server enforces `retries`, `timeout`, and
`queue` at submit and claim time.

## 3. Implement and register the handler

Handlers run in your service's binary. Register one against the job's
canonical id, unmarshalling the args JSON into your own struct:

```go
type importContactsArgs struct {
    AccountID      string `json:"account_id"`
    ImportStrategy string `json:"import_strategy"`
}

registry := jobs.NewRegistry()
registry.Register("directory.ImportContacts", jobs.HandlerFunc(
    func(ctx context.Context, argsJSON []byte) error {
        var args importContactsArgs
        if err := json.Unmarshal(argsJSON, &args); err != nil {
            return err
        }
        jobs.Checkpoint(ctx, 10, "fetching contacts")
        // ... your import logic ...
        return nil
    }))
```

A non-nil return retries the attempt up to the declared `retries`, then
dead-letters it. `jobs.Checkpoint(ctx, pct, msg)` reports progress and
extends the attempt's lease; long-running handlers call it as they go.

## 4. Run a worker

The worker holds a gRPC session to your organisation's server and receives
dispatched work over it — your service needs no database access:

```go
w := jobs.NewDispatchedWorker(conn, registry, "contacts", jobs.ServerConfig{
    Logger: slog.Default(),
})
go w.Run(ctx)
```

`conn` is your service's authenticated `*grpc.ClientConn` to atlantis.
`Run` reconnects on stream errors
with backoff; work in flight when a worker dies is re-dispatched to another
worker after its lease expires, so handlers must be idempotent.

## 5. Submit

```bash
tide job submit directory.ImportContacts \
  --args='{"account_id":"acct_123","import_strategy":"replace"}'
```

```
✔ submitted directory.ImportContacts as job 0198f2c1a4e07000
       monitor with: tide job status 0198f2c1a4e07000
```

Services submit through the same gRPC API (`SubmitJob`, requiring the
`JOBS_WRITE` capability), or atomically from a procedure:

```atl
procedure ConnectAccount for directory.Account {
  input { account_id: varchar(64) }
  steps {
    update Account set status = "connected" where account_id = $account_id
    enqueue directory.ImportContacts(account_id: $account_id, import_strategy: "replace")
  }
}
```

The enqueue shares the procedure's transaction: if the procedure rolls
back, the job is never enqueued. A brand-new procedure's RPC becomes
callable at the next server restart — see
[Custom queries and procedures](../concepts/custom-queries-and-procedures.md#adding-vs-editing).

## Verify

```bash
tide job status 0198f2c1a4e07000
```

```
job-id     0198f2c1a4e07000
job-name   directory.ImportContacts
queue      contacts
status     complete
attempts   1 / 3
enqueued   2026-09-02T14:03:11Z
```

The status reaches `complete`, with any checkpoints your handler reported.
The console's Workers page shows the connected worker session and its
queue.

## Next steps

- [Operate jobs and workflows](operate-jobs-and-workflows.md) — monitoring,
  the dead-letter queue, retries.
- [Long-running handlers](long-running-handlers.md) — idempotency, leases,
  and resume-from-progress in depth.
- [Jobs and workflows](../concepts/jobs-and-workflows.md) — the model.
