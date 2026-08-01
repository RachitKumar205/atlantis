# Recover a dropped table or column

Removing an entity or a field from a `.atl` file does not drop anything. The
object is **parked**: a table is moved into the `atlantis_tombstone` schema, a
column is renamed out of the way, and the rows stay exactly where they were.
It disappears from every read, write and generated type immediately, and it is
fully restorable for **30 days**. After that a background job drops it.

This guide is for the case where you removed something and want it back.

## Is it still there?

```sql
SELECT original_schema, original_name, kind, parked_at, reap_after
  FROM atlantis.parked_objects
 WHERE reaped_at IS NULL
 ORDER BY reap_after;
```

Each row is an object you can still recover, and `reap_after` is the instant it
stops being recoverable. That instant is fixed when the object is parked, so
changing the retention setting later never shortens a window something was
already promised.

Rows with `reaped_at` set are the audit trail: the object existed, and it was
dropped on that date. Those are gone — recovery is a restore from backup.

## Restoring it

Revert the schema change and apply. The down migration renames the object back,
restores any `NOT NULL` the park had to drop, and removes the registration:

```bash
git revert <the commit that removed it>
tide plan && tide apply
```

That is the whole procedure, and it is the one to prefer: it puts the schema and
the database back in agreement, which a manual rename does not.

If the retention window has already passed, the down migration fails with a
message saying so rather than reporting a success that restored nothing.

## Changing the window

`reap_after` is an ordinary column. To keep something longer:

```sql
UPDATE atlantis.parked_objects
   SET reap_after = now() + INTERVAL '90 days'
 WHERE original_name = 'orders' AND reaped_at IS NULL;
```

To reclaim the space now, set it into the past and let the reaper pick it up on
its next run:

```sql
UPDATE atlantis.parked_objects
   SET reap_after = now()
 WHERE original_name = 'orders' AND reaped_at IS NULL;
```

## When something will not reap

A drop can fail — most often because a view or foreign key was created against
the parked object after it was parked. The reaper does not use `CASCADE`, so it
declines rather than destroying whatever depends on it, records the reason, and
backs off:

```sql
SELECT original_name, attempts, next_attempt_after, last_error
  FROM atlantis.parked_objects
 WHERE reaped_at IS NULL AND attempts > 0;
```

Drop the dependency and the next run will succeed. Until then the object stays
parked, and — because failures back off — it does not block anything else from
being reaped.

## How the reap runs

`atlantis.ReapParked` is a built-in job on an hourly schedule, drained by a
worker on the `atlantis` queue that every server runs. Both are independent of
`ATL_JOBS_WORKER_ENABLED`, which governs caller job queues only.

To pause reaping entirely:

```sql
UPDATE atlantis.job_schedules SET enabled = false WHERE job_name = 'atlantis.ReapParked';
```

An operator's edit to that row survives deploys — the schedule is only ever
inserted, never overwritten.

## Related

- [Schema history](schema-history.md). What changed, when, and who applied it.
- [Deploy to production](deploy-to-production.md). Where apply sits in CI.
- [Jobs and workflows](../concepts/jobs-and-workflows.md). The runtime the
  reaper is a job on.
