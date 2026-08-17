# Row-level TTL

Declare a `ttl_field` on any entity with a `timestamptz not null` column, and atlantis automatically deletes expired rows on a 1-minute sweep.

## 1. Add the TTL column + directive

```atl
entity Session in consumer {
  id            varchar(8) primary
  consumer_id   varchar(8) not null references consumer.Account.id
  session_token varchar(255) not null unique
  expires_at    timestamptz not null
  created_at    timestamptz not null default now()

  ttl_field expires_at
}
```

`ttl_field` names the column the sweeper checks. The column must be `timestamptz` and `not null`.

## 2. Apply

```bash
tide apply
```

The `ttl_field` directive is recorded in the IR checkpoint. The built-in `SweepExpired` job reads the checkpoint at runtime to discover which entities have TTL columns.

## 3. Verify

Insert a row with `expires_at` in the past:

```sql
INSERT INTO consumer.sessions (id, consumer_id, session_token, expires_at)
VALUES ('s1', 'c1', 'tok', now() - interval '1 hour');
```

Within five minutes the sweeper deletes it. Check:

```sql
SELECT count(*) FROM consumer.sessions WHERE expires_at < now();
```

## How it works

- atlantis ships a built-in job `atlantis.SweepExpired` that runs on a `*/5 * * * *` cron schedule (every five minutes).
- On each fire, the sweeper loads the IR checkpoint, finds every entity with `ttl_field` set, and deletes up to 1000 expired rows per entity.
- The batch limit prevents vacuum churn; leftover rows get caught on the next sweep tick.
- A sweep that fails is reported to the job runtime, so it retries and eventually dead-letters rather than failing silently. Check `tide job dead` if rows are not disappearing.
- Operators can tune the cadence by updating `atlantis.job_schedules` directly (`UPDATE ... SET cron_spec = '*/5 * * * *'`) or disable with `enabled = false`.

## Expiring rows on a tenant-isolated table

A `DELETE` sweep cannot work on a table with tenant isolation. The sweeper is a background job with no request behind it, so it binds no tenant; row-level security still applies to its `DELETE`, `atlantis.current_partition()` is `NULL`, the statement matches nothing and reports success. Binding some tenant would not fix it — expiry has to cover every tenant, and there is no single correct value to bind.

**Declare the entity a hypertable on its TTL column, and expiry drops whole chunks instead.**

```atl
hypertable Event in shop on occurred_at {
  id          bigint not null
  tenant      varchar(32) not null
  occurred_at timestamptz not null
  body        text

  primary by id, occurred_at
  chunk_time_interval 1d
  partition by tenant
  ttl_field occurred_at
}
```

The primary key includes `occurred_at` because TimescaleDB refuses a unique index that does not contain the time column. A single-column `id primary` fails the apply with `SQLSTATE TS103`.

Dropping a chunk is DDL, and row-level security filters queries, not `DROP TABLE`. So this needs no tenant bound, no registry of tenants to iterate, and no database role exempt from the policy. It is also far cheaper: one operation per chunk rather than one per row.

### The TTL column must be the time dimension

`ttl_field` has to name the same column the hypertable is declared `on`. Chunks are selected by the time dimension, so if `ttl_field` named a different column a chunk whose time range has passed could still hold rows whose TTL has not — and dropping it would delete live data.

`tide apply` refuses any other combination of `partition by` and `ttl_field`, rather than accepting a retention rule it would silently not honour. The error names all three ways out: declare the hypertable, drop `partition by`, or expire from your caller.

### Granularity

A chunk is dropped only once its **entire** time range is in the past, so rows can outlive their TTL by up to one `chunk_time_interval`. Choose the interval for the retention precision you need — `1d` means a row expires within a day of its TTL, `1h` within an hour.

### What to watch

```
atlantis_sweeper_chunks_dropped_total{entity="shop.Event"}
```

Incremented on every sweep, including by zero, so `rate() == 0` on a hypertable that should be ageing out is a question you can alert on.

The older counter still exists for entities carrying a checkpoint written before apply started refusing them:

```
atlantis_sweeper_sweeps_blocked_total{entity="shop.Session"}
```

**Any non-zero value there means expired rows are accumulating.** Alert on it.

## Related

- [Jobs and workflows concept](../concepts/jobs-and-workflows.md). The sweeper is itself a job running on the atlantis runtime.
- [Declarative jobs guide](declarative-jobs.md). How to declare and run your own jobs.
