# Jobs and workflows

Jobs are typed background-work declarations. You declare the args a handler receives, how many times to retry on failure, and how long each attempt can run. atlantis validates submissions against the declaration, queues them durably, and routes claimed work to the handler your own service registers — handlers run in your binary, not in atlantis.

```atl
job ImportContacts in directory {
  args {
    account_id      varchar(64) not null
    import_strategy varchar(20) not null default "skip"
  }
  retries  3
  timeout  30m
  queue    "contacts"
}
```

Workflows are multi-step orchestrations built on top of jobs. Each step runs a declared job; steps execute in declaration order. If a step fails after exhausting retries, compensations for prior steps run in reverse to undo the work.

```atl
workflow OnboardAccount in directory {
  state {
    account_id varchar(64) not null
    provider   varchar(255) not null
  }
  step import_contacts {
    job ImportContacts
    args { account_id: $account_id, import_strategy: "replace" }
  }
  compensate import_contacts {
    job PurgeContacts
    args { account_id: $account_id }
  }
}
```

## How jobs run

A submission — over the admin API, from a procedure's `enqueue` step, or from `tide job submit` — becomes a durable queue row. A worker claims the row; claims are exclusive, so two workers never run the same attempt. The handler runs under the declared per-attempt timeout, and a heartbeat extends the claim's lease so a live handler is not claimed twice. On success the job completes; on failure it retries up to `retries`, then moves to the dead-letter queue, visible in the console's Operations page and `tide job dead`.

## Handler registration

Handlers live in your service's binary. `tide generate` writes a typed surface per declared job into the generated client tree — an `Args` struct, a handler interface, and a `Register<Job>` helper that decodes the args JSON before invoking your handler:

```go
directory.RegisterImportContacts(registry, &importContactsHandler{crm: crmClient})
```

Registering under the raw job id with `jobs.HandlerFunc` works too, decoding the JSON yourself. [Declare a background job](../guides/declarative-jobs.md) walks through the whole loop, including the worker that receives dispatched claims.

## Submission paths

| Path | When to use |
|---|---|
| Admin API `SubmitJob` | From a service: the job's full id plus the args as JSON, over gRPC. |
| Procedure `enqueue` step | Atomic with a write: the job is enqueued in the procedure's transaction, or not at all. |
| `tide job submit` | Ad-hoc submission from a terminal, recorded under `cli:<user>`. |
| Declared `schedule` | The platform fires the job on its cron cadence, recorded under `atlantis.scheduler`. |

## Runtime modifiers

| Modifier | Default | Effect |
|---|---|---|
| `retries N` | 0 | Attempts before the dead-letter queue. |
| `timeout 30m` | 30m | Per-attempt deadline. `timeout none` removes it. |
| `heartbeat 10m` | 5m | Per-attempt lease window. Widen for handlers that block on one long external call; narrow to fail fast on quick jobs. |
| `queue "name"` | `"default"` | Named queue for partitioning worker pools. |
| `schedule "cron"` | (none) | Fires the job on the 5-field cron cadence, with no args. A fired run carries the job's queue, retries, and timeout, and skips while a previous run is still pending. |
| `visible_to "caller"` | (any) | Only the named caller may submit the job or claim it with its workers. `"*"` means any; aliases count. A procedure's `enqueue` step is not gated by it. |

## Checkpointing

Long-running handlers call `jobs.Checkpoint(ctx, pct, msg)` to report progress. Each call bumps the claim's lease and persists the progress, which `tide job status` and the console's worker detail show live. `Checkpoint` returns an error. A failed progress write never fails the claim, so handlers can discard it.

See [Long-running handlers](../guides/long-running-handlers.md) for the full handler contract — idempotency, the heartbeat / checkpoint distinction, resume-from-progress, and a worked contact-import example.

## Distributed tracing

When the caller has an active OpenTelemetry span, `SubmitJob` captures the W3C traceparent into the row's `trace_ctx` column. The worker resumes the trace on claim and starts a child span around `handler.Handle`, so the submit-side and worker-side spans stitch into one distributed trace in any W3C trace-context backend.

## How workflows run

Starting a workflow records an instance and enqueues the first step's job. Each completion enqueues the next step, and the workflow is `complete` when the last step finishes. If any step's job exhausts its retries, the workflow is marked `failed` and compensations for the completed steps are enqueued in reverse order.

Compensations are themselves jobs: at the moment the workflow reports `failed`, they may still be running, and a compensation that fails dead-letters like any other job.

## Crash recovery

If a worker dies mid-handler, the claim's lease expires and another worker claims the attempt on its next pass, running the handler from scratch. Each claim increments the attempt count, so handlers must be idempotent — the same input processed twice must land in the same state.

## Related

- [Declarative jobs guide](../guides/declarative-jobs.md). Step-by-step recipe for declaring and running a job.
- [Operate jobs and workflows](../guides/operate-jobs-and-workflows.md). Submitting, monitoring, and the dead-letter queue.
- [Custom queries and procedures](custom-queries-and-procedures.md). The synchronous counterpart to jobs.
- [DSL grammar reference](../reference/dsl-grammar.md). Full grammar including `job`, `workflow`, and `enqueue`.
