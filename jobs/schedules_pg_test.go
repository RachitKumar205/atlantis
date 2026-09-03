package jobs

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// A declared `schedule` clause lands in atlantis.job_schedules with the
// job's queue, retry budget, timeout, and owner — everything a fire
// writes into atlantis.jobs.
func TestSyncDeclaredSchedulesWritesTheRow(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "syncns.SyncedJob"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
	})

	ir := &dsl.IR{Jobs: []dsl.Job{{
		Name: "SyncedJob", Namespace: "syncns",
		Schedule: "*/10 * * * *", Queue: "contacts",
		Retries: 3, TimeoutMS: 120000, VisibleTo: "directory",
	}}}
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var (
		spec, queue, owner, managedBy string
		retries                       int
		timeoutMS                     *int
		enabled                       bool
	)
	if err := pool.QueryRow(ctx, `
SELECT cron_spec, queue, owner_caller, managed_by, max_retries, timeout_ms, enabled
  FROM atlantis.job_schedules WHERE job_name = $1`, jobName).
		Scan(&spec, &queue, &owner, &managedBy, &retries, &timeoutMS, &enabled); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if spec != "*/10 * * * *" || queue != "contacts" || owner != "directory" ||
		managedBy != "dsl" || retries != 3 || timeoutMS == nil || *timeoutMS != 120000 || !enabled {
		t.Errorf("row = spec %q queue %q owner %q managed_by %q retries %d timeout %v enabled %v",
			spec, queue, owner, managedBy, retries, timeoutMS, enabled)
	}
}

// A clause that disappeared from the checkpoint deletes its row; the
// built-ins' rows are not touched.
func TestSyncDeclaredSchedulesPrunesRemovedClauses(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "syncns.PrunedJob"
	const builtinName = "syncns.HandBuiltRow"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = ANY($1)`,
			[]string{jobName, builtinName})
	})

	ir := &dsl.IR{Jobs: []dsl.Job{{
		Name: "PrunedJob", Namespace: "syncns", Schedule: "*/10 * * * *",
	}}}
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, default_args)
VALUES ($1, '17 * * * *', '{}')`, builtinName); err != nil {
		t.Fatalf("seed builtin-style row: %v", err)
	}

	if err := SyncDeclaredSchedules(ctx, pool, &dsl.IR{}, quietLogger()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.job_schedules WHERE job_name = $1`, jobName).Scan(&n); err != nil {
		t.Fatalf("count pruned: %v", err)
	}
	if n != 0 {
		t.Error("a removed schedule clause left its row behind")
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.job_schedules WHERE job_name = $1`, builtinName).Scan(&n); err != nil {
		t.Fatalf("count builtin: %v", err)
	}
	if n != 1 {
		t.Error("the sync pruned a managed_by='builtin' row")
	}
}

// Disabling a schedule is an operator decision. A redeploy re-syncing
// the same clause updates the cadence and leaves enabled=false alone.
func TestSyncDeclaredSchedulesKeepsAnOperatorDisable(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "syncns.DisabledJob"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
	})

	ir := &dsl.IR{Jobs: []dsl.Job{{
		Name: "DisabledJob", Namespace: "syncns", Schedule: "*/10 * * * *",
	}}}
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE atlantis.job_schedules SET enabled = false WHERE job_name = $1`, jobName); err != nil {
		t.Fatalf("disable: %v", err)
	}

	ir.Jobs[0].Schedule = "*/20 * * * *"
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("re-sync: %v", err)
	}

	var spec string
	var enabled bool
	if err := pool.QueryRow(ctx, `
SELECT cron_spec, enabled FROM atlantis.job_schedules WHERE job_name = $1`, jobName).
		Scan(&spec, &enabled); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if spec != "*/20 * * * *" {
		t.Errorf("cron_spec = %q, want the re-synced cadence", spec)
	}
	if enabled {
		t.Error("re-syncing a clause re-enabled a schedule an operator disabled")
	}
}

// An unparseable spec is skipped, not synced and not fatal: the other
// clauses still sync.
func TestSyncDeclaredSchedulesSkipsABadCronSpec(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const good = "syncns.GoodSpec"
	const bad = "syncns.BadSpec"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = ANY($1)`,
			[]string{good, bad})
	})

	ir := &dsl.IR{Jobs: []dsl.Job{
		{Name: "BadSpec", Namespace: "syncns", Schedule: "not a cron"},
		{Name: "GoodSpec", Namespace: "syncns", Schedule: "*/10 * * * *"},
	}}
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.job_schedules WHERE job_name = $1`, bad).Scan(&n); err != nil {
		t.Fatalf("count bad: %v", err)
	}
	if n != 0 {
		t.Error("an unparseable spec was synced anyway")
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.job_schedules WHERE job_name = $1`, good).Scan(&n); err != nil {
		t.Fatalf("count good: %v", err)
	}
	if n != 1 {
		t.Error("a bad spec stopped the good one from syncing")
	}
}

// A fired row carries the schedule row's queue, retry budget, timeout,
// and owner into atlantis.jobs — the declared job's runtime settings,
// not the built-ins'.
func TestScheduleFiresWithTheRowsRuntimeSettings(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "syncns.FiredWithSettings"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = $1`, jobName)
	}
	clean()
	t.Cleanup(clean)

	// Every-minute cadence, last fired two minutes ago: due now.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules
    (job_name, cron_spec, last_fired_at, queue, max_retries, timeout_ms, owner_caller, managed_by)
VALUES ($1, '* * * * *', now() - INTERVAL '2 minutes', 'contacts', 3, 120000, 'directory', 'dsl')`,
		jobName); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s := &Scheduler{Pool: pool, Logger: quietLogger()}
	if err := s.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var (
		queue, owner, submittedBy string
		maxRetries                int
		timeoutMS                 *int
	)
	if err := pool.QueryRow(ctx, `
SELECT queue, owner_caller, submitted_by, max_retries, timeout_ms
  FROM atlantis.jobs WHERE job_name = $1`, jobName).
		Scan(&queue, &owner, &submittedBy, &maxRetries, &timeoutMS); err != nil {
		t.Fatalf("read fired job: %v", err)
	}
	if queue != "contacts" || owner != "directory" || maxRetries != 3 ||
		timeoutMS == nil || *timeoutMS != 120000 || submittedBy != "atlantis.scheduler" {
		t.Errorf("fired row = queue %q owner %q retries %d timeout %v submitted_by %q",
			queue, owner, maxRetries, timeoutMS, submittedBy)
	}
}

// A declared job whose id collides with a built-in's schedule row must
// not hijack it: the built-in's queue and timeout stay, and the clause
// is reported rather than synced.
func TestSyncDeclaredSchedulesDoesNotHijackABuiltinRow(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "syncns.TakenByBuiltin"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
	})

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, default_args, queue, timeout_ms)
VALUES ($1, '17 * * * *', '{}', 'atlantis', 600000)`, jobName); err != nil {
		t.Fatalf("seed builtin row: %v", err)
	}

	ir := &dsl.IR{Jobs: []dsl.Job{{
		Name: "TakenByBuiltin", Namespace: "syncns",
		Schedule: "*/10 * * * *", Queue: "contacts", Retries: 5,
	}}}
	if err := SyncDeclaredSchedules(ctx, pool, ir, quietLogger()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var spec, queue, managedBy string
	if err := pool.QueryRow(ctx, `
SELECT cron_spec, queue, managed_by FROM atlantis.job_schedules WHERE job_name = $1`, jobName).
		Scan(&spec, &queue, &managedBy); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if spec != "17 * * * *" || queue != "atlantis" || managedBy != "builtin" {
		t.Errorf("the sync rewrote a builtin row: spec %q queue %q managed_by %q",
			spec, queue, managedBy)
	}

	// And the prune must not delete it either.
	if err := SyncDeclaredSchedules(ctx, pool, &dsl.IR{}, quietLogger()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM atlantis.job_schedules WHERE job_name = $1`, jobName).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Error("the prune deleted a builtin row after a name collision")
	}
}
