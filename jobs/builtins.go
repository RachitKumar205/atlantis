package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Built-in jobs: the background work atlantis performs on its own behalf,
// as opposed to the handlers a caller registers through the generated SDK.
//
// A background job is uniquely bad at announcing that it is absent. A forgotten
// RPC returns an error; a forgotten job produces the same silence as a job with
// nothing to do — an unregistered TTL sweeper leaves the rows it should have
// deleted in place, with no log line and no failing table.
//
// So the wiring is the list below rather than a call site in main, and
// builtins_test.go fails when a handler in this package appears in neither that
// list nor unscheduledBuiltins.

// BuiltinQueue is the queue built-in jobs are enqueued to and drained from.
//
// Separate from the caller queues named in ATL_JOBS_QUEUES because the two are
// governed differently: caller queues are opt-in and an operator may run none
// of them, while atlantis's own maintenance has to run wherever atlantis runs.
// Parked objects are retained for thirty days on the promise that something
// eventually removes them, and a promise conditional on an unrelated feature
// flag is not one worth making.
const BuiltinQueue = "atlantis"

// Builtin is one job atlantis ships and operates itself.
type Builtin struct {
	// Name is the job id, matching atlantis.job_schedules.job_name.
	Name string

	// CronSpec is the default cadence. Operators may UPDATE the row or set
	// enabled = false; the schedule is only ever inserted, never overwritten,
	// so a deliberate change survives every restart.
	CronSpec string

	// New builds the handler. Takes the pool and logger because every built-in
	// so far needs both and neither is available at package scope.
	New func(pool *pgxpool.Pool, logger *slog.Logger) Handler

	// Why records what the job is for, in one line, for a reader deciding
	// whether a schedule row can be disabled.
	Why string
}

// Builtins lists every job the server registers and schedules for itself.
//
// A handler in this package that belongs nowhere here goes in
// unscheduledBuiltins (builtins_test.go) with a reason. The test enforces that
// the two lists together cover every handler.
func Builtins() []Builtin {
	return []Builtin{
		{
			Name:     SweepExpiredJobName,
			CronSpec: "*/5 * * * *",
			Why: "deletes rows whose ttl_field has passed, for every entity " +
				"declaring one",
			New: func(pool *pgxpool.Pool, logger *slog.Logger) Handler {
				h := &SweepExpiredHandler{
					Pool:       pool,
					Logger:     logger.With("job", SweepExpiredJobName),
					BatchLimit: 1000,
				}
				return HandlerFunc(h.Handle)
			},
		},
		{
			Name:     ReapParkedJobName,
			CronSpec: "17 * * * *",
			Why: "drops objects parked by a destructive migration once their " +
				"retention window has passed",
			New: func(pool *pgxpool.Pool, logger *slog.Logger) Handler {
				h := &ReapParkedHandler{
					Pool:        pool,
					Logger:      logger.With("job", ReapParkedJobName),
					BatchLimit:  50,
					LockTimeout: DefaultReapLockTimeout,
				}
				return HandlerFunc(h.Handle)
			},
		},
	}
}

// RegisterBuiltins registers every built-in handler and installs its schedule.
//
// Both halves in one call: a registered handler with no schedule never fires,
// and a schedule with no registered handler fails every tick with "handler not
// registered". Two functions can be called singly.
func RegisterBuiltins(ctx context.Context, reg *Registry, pool *pgxpool.Pool, logger *slog.Logger) error {
	for _, b := range Builtins() {
		reg.Register(b.Name, b.New(pool, logger))
		if err := ensureSchedule(ctx, pool, b); err != nil {
			return fmt.Errorf("schedule %s: %w", b.Name, err)
		}
		logger.Info("built-in job registered", "job", b.Name, "cron", b.CronSpec)
	}
	return nil
}

// ensureSchedule inserts the job's schedule row if it is absent.
//
// DO NOTHING rather than DO UPDATE. A changed cadence or a disabled job is a
// deliberate edit, and a deploy resetting it reads the same as one that never
// touched the row.
func ensureSchedule(ctx context.Context, pool *pgxpool.Pool, b Builtin) error {
	args, err := json.Marshal(struct{}{})
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules (job_name, cron_spec, default_args, queue, timeout_ms)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (job_name) DO NOTHING`,
		b.Name, b.CronSpec, args, BuiltinQueue, int(scheduledJobTimeout/time.Millisecond))
	return err
}
