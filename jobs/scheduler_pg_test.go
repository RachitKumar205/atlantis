package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func schedTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the scheduler")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The whole chain: a due row in atlantis.job_schedules becomes a job in
// atlantis.jobs, and a worker runs the registered handler.
//
// This is the test whose absence was the bug. Every other test in this package
// checks one link — the handler is listed, the list is called, the handler
// works when invoked directly — and all of them passed while the chain led
// nowhere, because nothing read job_schedules at all. Only driving it end to
// end distinguishes "wired" from "wired to nothing".
func TestScheduleFiresAndReachesItsHandler(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "atlantis.SchedTestJob"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs_dead WHERE job_name = $1`, jobName)
	}
	clean()
	t.Cleanup(clean)

	// Due: it last fired two minutes ago on an every-minute schedule.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, default_args, last_fired_at)
VALUES ($1, '* * * * *', '{"marker":"scheduled"}'::jsonb, now() - INTERVAL '2 minutes')`,
		jobName); err != nil {
		t.Fatalf("insert schedule: %v", err)
	}

	ran := make(chan []byte, 4)
	reg := NewRegistry()
	reg.Register(jobName, HandlerFunc(func(ctx context.Context, args []byte) error {
		ran <- args
		return nil
	}))

	s := &Scheduler{Pool: pool, Logger: quietLogger()}
	if err := s.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var queued int
	var queue string
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(queue), '') FROM atlantis.jobs WHERE job_name = $1`,
		jobName).Scan(&queued, &queue); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued != 1 {
		t.Fatalf("%d jobs enqueued from a due schedule, want 1. A schedule row "+
			"that produces no job is the whole failure this component exists to "+
			"fix: nothing errors, nothing logs, the work simply never happens", queued)
	}
	if queue != BuiltinQueue {
		t.Errorf("enqueued onto queue %q, want %q — a worker draining the "+
			"built-in queue would never see it", queue, BuiltinQueue)
	}

	// And a worker on that queue must actually reach the handler.
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := NewWorker(pool, reg, BuiltinQueue, Config{
		Schema:        "atlantis",
		DrainInterval: 200 * time.Millisecond,
		BatchSize:     5,
		Logger:        quietLogger(),
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = w.Run(workerCtx) }()

	select {
	case args := <-ran:
		// Decoded, not compared byte-for-byte: the value round-trips through
		// jsonb, which normalises whitespace and key order.
		var got map[string]string
		if err := json.Unmarshal(args, &got); err != nil {
			t.Errorf("handler received args that are not valid JSON (%q): %v", args, err)
		} else if got["marker"] != "scheduled" {
			t.Errorf("handler received args %q, want the schedule's default_args. "+
				"A scheduled job that loses its arguments runs with the wrong "+
				"input and nothing reports it", args)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the job was enqueued but no worker ever ran its handler. The " +
			"schedule fires into a queue nobody drains, which is the same " +
			"outcome as no scheduler at all")
	}
	cancel()
	wg.Wait()
}

// A schedule that is not yet due must not fire.
//
// Without this the scheduler is just "enqueue everything every tick", which
// passes the test above and is catastrophically wrong.
func TestScheduleDoesNotFireBeforeItIsDue(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "atlantis.SchedNotDueJob"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = $1`, jobName)
	}
	clean()
	t.Cleanup(clean)

	// Daily at 03:00, fired a minute ago: the next firing is nearly a day out.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, last_fired_at)
VALUES ($1, '0 3 * * *', now() - INTERVAL '1 minute')`, jobName); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s := &Scheduler{Pool: pool, Logger: quietLogger()}
	for i := 0; i < 3; i++ {
		if err := s.tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.jobs WHERE job_name = $1`, jobName).Scan(&queued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued != 0 {
		t.Errorf("%d jobs enqueued for a schedule that is not due. The scheduler "+
			"is firing on every tick regardless of the cron spec", queued)
	}
}

// A disabled schedule must not fire, and a never-fired one is armed rather
// than fired.
func TestScheduleRespectsEnabledAndArmsOnFirstSight(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const off = "atlantis.SchedDisabledJob"
	const fresh = "atlantis.SchedFreshJob"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = ANY($1)`,
			[]string{off, fresh})
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = ANY($1)`,
			[]string{off, fresh})
	}
	clean()
	t.Cleanup(clean)

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, enabled, last_fired_at) VALUES
  ($1, '* * * * *', false, now() - INTERVAL '2 minutes'),
  ($2, '* * * * *', true,  NULL)`, off, fresh); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s := &Scheduler{Pool: pool, Logger: quietLogger()}
	if err := s.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	var offQueued, freshQueued int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE job_name = $1), count(*) FILTER (WHERE job_name = $2)
  FROM atlantis.jobs`, off, fresh).Scan(&offQueued, &freshQueued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if offQueued != 0 {
		t.Error("a schedule with enabled=false fired. Disabling a job is the only " +
			"lever an operator has to stop it")
	}
	if freshQueued != 0 {
		t.Error("a schedule that had never fired ran immediately. On a fresh " +
			"database that means every built-in job fires at once at boot")
	}

	// Armed, so it fires on its next real occurrence rather than never.
	var armed bool
	if err := pool.QueryRow(ctx,
		`SELECT last_fired_at IS NOT NULL FROM atlantis.job_schedules WHERE job_name = $1`,
		fresh).Scan(&armed); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !armed {
		t.Error("a never-fired schedule was left unarmed, so it is re-evaluated " +
			"from scratch on every tick and never establishes a baseline")
	}
}

// Two replicas must not both fire the same schedule.
//
// Every pod runs a scheduler; without the advisory-lock election a three-pod
// deployment enqueues three copies of every scheduled job, and for the reaper
// that means three concurrent ACCESS EXCLUSIVE attempts on the same live table.
func TestOnlyOneSchedulerFiresASchedule(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "atlantis.SchedRaceJob"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = $1`, jobName)
	}
	clean()
	t.Cleanup(clean)

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, last_fired_at)
VALUES ($1, '* * * * *', now() - INTERVAL '2 minutes')`, jobName); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Separate pools, because the advisory lock is session-scoped and two
	// schedulers sharing a pool could be handed the same session.
	var pools []*pgxpool.Pool
	for i := 0; i < 4; i++ {
		p, err := pgxpool.New(ctx, os.Getenv("ATLANTIS_TEST_PG"))
		if err != nil {
			t.Fatalf("pool %d: %v", i, err)
		}
		defer p.Close()
		pools = append(pools, p)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, p := range pools {
		wg.Add(1)
		go func(p *pgxpool.Pool) {
			defer wg.Done()
			<-start
			s := &Scheduler{Pool: p, Logger: quietLogger()}
			_ = s.tick(ctx)
		}(p)
	}
	close(start)
	wg.Wait()

	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.jobs WHERE job_name = $1`, jobName).Scan(&queued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued != 1 {
		t.Errorf("%d jobs enqueued by 4 concurrent schedulers, want exactly 1. "+
			"Every replica runs a scheduler, so without single-leader election "+
			"a scheduled job runs once per pod", queued)
	}
}

// A job whose previous run is still outstanding must not be enqueued again.
//
// This is what bounds the damage when a handler is missing or wedged: without
// it, an hourly schedule with no working handler adds a row every hour
// forever.
func TestScheduleDoesNotPileUpOnAStuckRun(t *testing.T) {
	pool, ctx := schedTestPool(t)
	const jobName = "atlantis.SchedPileUpJob"

	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.job_schedules WHERE job_name = $1`, jobName)
		_, _ = pool.Exec(ctx, `DELETE FROM atlantis.jobs WHERE job_name = $1`, jobName)
	}
	clean()
	t.Cleanup(clean)

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, last_fired_at)
VALUES ($1, '* * * * *', now() - INTERVAL '2 minutes')`, jobName); err != nil {
		t.Fatalf("insert: %v", err)
	}

	s := &Scheduler{Pool: pool, Logger: quietLogger()}
	// No worker is running, so the first fire stays pending forever — exactly
	// the situation an unregistered handler produces.
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(ctx,
			`UPDATE atlantis.job_schedules SET last_fired_at = now() - INTERVAL '2 minutes'
			  WHERE job_name = $1`, jobName); err != nil {
			t.Fatalf("rewind: %v", err)
		}
		if err := s.tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}

	var queued int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM atlantis.jobs WHERE job_name = $1`, jobName).Scan(&queued); err != nil {
		t.Fatalf("count: %v", err)
	}
	if queued != 1 {
		t.Errorf("%d jobs queued after 5 due ticks with nothing draining them, "+
			"want 1. A schedule whose handler is missing or wedged would grow "+
			"atlantis.jobs without bound", queued)
	}
}
