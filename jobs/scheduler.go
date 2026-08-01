package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The scheduler: the component that turns atlantis.job_schedules from a table
// into firing jobs.
//
// migrations/infra/0006_jobs.up.sql described this component in 2 paragraphs,
// jobs/registry.go documented its advisory-lock election, and it was never
// written. The consequences were invisible in exactly the way a missing
// background job always is: `ttl_field` deleted nothing, the DSL's `schedule`
// modifier parsed and validated and produced no fires, and docs/guides/row-ttl.md
// shipped with a "Verify" step promising a sweep that could not happen. Nothing
// errored, because nothing ran.
//
// Design notes that are load-bearing:
//
//   - ONE replica evaluates at a time, held by a session-level advisory lock.
//     Without it, every replica reads the same due row and enqueues its own
//     copy, so a three-pod deployment runs the reaper three times an hour.
//     The lock is taken per tick and released at the end of it, so a pod that
//     dies mid-tick frees it when its connection closes rather than wedging
//     the fleet.
//
//   - A missed window fires ONCE, not once per missed occurrence. A server
//     down for six hours with an hourly job comes back and enqueues one job,
//     not six. Catch-up is the wrong default for periodic maintenance: the
//     work is idempotent and the six copies would all do the same scan.
//
//   - A schedule with a job already pending does not enqueue another. This is
//     what stops an unregistered or wedged handler turning a schedule into
//     unbounded queue growth — the failure mode that makes a missing handler
//     much worse than a missing schedule.
type Scheduler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger

	// Interval between evaluations. Cron resolves to the minute, so anything
	// under a minute is enough; shorter ticks only shorten the delay between a
	// fire becoming due and being enqueued.
	Interval time.Duration
}

// DefaultSchedulerInterval is how often schedules are evaluated.
const DefaultSchedulerInterval = 20 * time.Second

// schedulerLockKey is the advisory-lock key electing the evaluating replica.
// An arbitrary constant, fixed forever: two atlantis versions running side by
// side during a rolling deploy must contend for the SAME lock, or both will
// think they are the leader.
const schedulerLockKey int64 = 8021977453110001

// Run evaluates schedules until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultSchedulerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.log().Info("job scheduler started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				// A tick that fails is not fatal: the next one re-reads
				// everything from the table, so a transient database error
				// costs one evaluation rather than the scheduler.
				s.log().Warn("job scheduler tick failed", "err", err)
			}
		}
	}
}

// tick evaluates every enabled schedule once, if this replica wins the lock.
func (s *Scheduler) tick(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// Session-level, on this one connection, so it is released by the UNLOCK
	// below or by the connection dying — never held past the life of the pod.
	var leader bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, schedulerLockKey).
		Scan(&leader); err != nil {
		return fmt.Errorf("elect: %w", err)
	}
	if !leader {
		return nil
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, schedulerLockKey); err != nil {
			// Not fatal — the lock dies with the session — but worth saying,
			// because a leaked lock stalls scheduling until this pod restarts.
			s.log().Warn("job scheduler could not release its lock", "err", err)
		}
	}()

	rows, err := conn.Query(ctx, `
SELECT id, job_name, cron_spec, default_args, last_fired_at
  FROM atlantis.job_schedules
 WHERE enabled
 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list schedules: %w", err)
	}
	type row struct {
		id        int64
		jobName   string
		spec      string
		args      []byte
		lastFired *time.Time
	}
	var schedules []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.jobName, &r.spec, &r.args, &r.lastFired); err != nil {
			rows.Close()
			return fmt.Errorf("scan schedule: %w", err)
		}
		schedules = append(schedules, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schedules: %w", err)
	}

	now := time.Now().UTC()
	for _, r := range schedules {
		cron, err := parseCron(r.spec)
		if err != nil {
			// Loud, and every tick: an unparseable spec means this job will
			// never fire, and the operator who typed it has no other signal.
			s.log().Error("job schedule has an unparseable cron spec and will "+
				"never fire; fix atlantis.job_schedules.cron_spec or disable the row",
				"job", r.jobName, "spec", r.spec, "err", err)
			continue
		}

		// A schedule that has never fired is armed rather than fired. Firing
		// immediately would mean every deploy of a fresh database runs every
		// built-in job at boot, all at once.
		if r.lastFired == nil {
			if _, err := conn.Exec(ctx,
				`UPDATE atlantis.job_schedules SET last_fired_at = now() WHERE id = $1`,
				r.id); err != nil {
				s.log().Warn("could not arm schedule", "job", r.jobName, "err", err)
			}
			continue
		}

		due, err := cron.next(r.lastFired.UTC())
		if err != nil {
			s.log().Error("job schedule has no reachable firing time",
				"job", r.jobName, "spec", r.spec, "err", err)
			continue
		}
		if due.After(now) {
			continue
		}
		if err := s.fire(ctx, conn, r.id, r.jobName, r.args); err != nil {
			s.log().Warn("could not enqueue scheduled job", "job", r.jobName, "err", err)
		}
	}
	return nil
}

// fire enqueues one run and records it, in one transaction.
//
// Together, so that a crash between the two cannot either drop the run or
// replay it on every tick forever.
func (s *Scheduler) fire(ctx context.Context, conn *pgxpool.Conn, id int64, jobName string, args []byte) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Skip if a previous run is still outstanding. A handler that is missing,
	// wedged, or simply slower than its cadence would otherwise accumulate one
	// row per tick forever — which turns a broken job into a broken database.
	var pending int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM atlantis.jobs
 WHERE job_name = $1 AND status IN ('pending', 'running')`, jobName).Scan(&pending); err != nil {
		return fmt.Errorf("check pending: %w", err)
	}
	if pending > 0 {
		// Still record the fire: otherwise `next` keeps resolving to the same
		// overdue instant and every tick re-checks it.
		if _, err := tx.Exec(ctx,
			`UPDATE atlantis.job_schedules SET last_fired_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		scheduleFiresTotal.WithLabelValues(jobName, "skipped").Inc()
		s.log().Info("scheduled job skipped: a previous run is still outstanding",
			"job", jobName, "outstanding", pending)
		return tx.Commit(ctx)
	}

	if len(args) == 0 {
		args = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO atlantis.jobs (job_name, queue, args, max_retries, timeout_ms, submitted_by)
VALUES ($1, $2, $3, 0, $4, 'atlantis.scheduler')`,
		jobName, BuiltinQueue, args, int(scheduledJobTimeout/time.Millisecond)); err != nil {
		return fmt.Errorf("enqueue: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE atlantis.job_schedules SET last_fired_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("record fire: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	scheduleFiresTotal.WithLabelValues(jobName, "fired").Inc()
	s.log().Info("scheduled job enqueued", "job", jobName, "queue", BuiltinQueue)
	return nil
}

// scheduledJobTimeout bounds one run of a scheduled job. Generous, because
// these are maintenance scans; the point is that a wedged run eventually
// releases its lease rather than blocking its schedule forever.
const scheduledJobTimeout = 10 * time.Minute

func (s *Scheduler) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
