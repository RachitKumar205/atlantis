# Operate jobs and workflows

Watch running work, drain the dead-letter queue, and drive workflows from
the terminal and the console.

## Prerequisites

- A declared job with a running worker
  ([Declare a background job](declarative-jobs.md)).
- Your caller holds the `JOBS_READ` capability for listings and status,
  and `JOBS_WRITE` for submit and retry.

## Watch a job

```bash
tide job status <job-id>
```

```
job-id     0198f2c1a4e07000
job-name   directory.ImportContacts
queue      contacts
status     running
attempts   1 / 3
enqueued   2026-09-02T14:03:11Z
```

Any progress the handler checkpointed follows the row. Exit 1 means the
job was not found or has `failed`; exit 0 covers every live or completed
state; exit 3 is an operational error (no connection, bad flags).

The console's **Workers** page shows each connected worker session with its
queue and in-flight work; a session's detail page can drain it (finish
in-flight work, take no more) or evict it.

## Drain the dead-letter queue

A job that exhausts its retries moves to the dead-letter queue with its
last error:

```bash
tide job dead --job-name=directory.ImportContacts
tide job retry <dead-job-id>
```

`retry` requeues the work as a new job:

```
✔ requeued dead job 0198f2c1a4e07000 -> 0198f31b22c07000
       monitor with: tide job status 0198f31b22c07000
```

The console's **Operations** page lists the same queue with per-job retry
(admin role).

Fix the cause before retrying: a dead job replays the same args against
the same handler.

## Drive a workflow

```bash
tide workflow start directory.OnboardAccount --state='{"account_id":"acct_123"}'
tide workflow status <workflow-id>
```

`status` prints the workflow's id, name, status, current step, and any
error; `--format=json` adds the state document. Exit 1 means not found or
`failed`; exit 3 is an operational error. A `failed` workflow has enqueued
compensations for its completed steps — they may still be running when the
status first reports `failed` — and the diagnostic says which step stopped
it.

## Watch a backfill

A backfill-required schema change applied with `tide apply --backfill`
runs its data phase in the background:

```bash
tide backfill status
```

`tide backfill status` with no argument reports this caller's latest
backfill plan: per-field row counts, the last key processed, and any
error. Exit 1 means the plan `failed`. The final phase — `SET NOT NULL`
and `DROP INDEX` — runs automatically once every field completes.

## Verify

After a retry, `tide job status <new-id>` reaches `complete`, and the
dead-letter listing no longer shows the original.

## Related

- [Jobs and workflows](../concepts/jobs-and-workflows.md) — retries,
  leases, compensations.
- [`tide` CLI](../reference/cli-tide.md) — flags and exit codes for every
  command above.
