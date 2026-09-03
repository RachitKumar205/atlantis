package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// SyncDeclaredSchedules mirrors the checkpoint's `schedule` clauses into
// atlantis.job_schedules, so the scheduler fires declared jobs the way it
// fires built-ins.
//
// A declared schedule upserts a managed_by='dsl' row carrying the job's
// queue, retry budget, timeout, and owner; a clause that disappeared from
// the checkpoint deletes its row. `enabled` is never written on an
// existing row — disabling a schedule is an operator decision a redeploy
// must not undo.
//
// A spec parseCron rejects is skipped and logged rather than failing the
// sync: one bad clause must not stop every other schedule from syncing.
//
// The fired row's owner_caller is the job's visible_to caller, so the
// declaring caller can read the run back; visible_to "*" or absent leaves
// the row operator-scoped.
func SyncDeclaredSchedules(ctx context.Context, pool *pgxpool.Pool, ir *dsl.IR, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if ir == nil {
		ir = &dsl.IR{}
	}

	keep := make([]string, 0, len(ir.Jobs))
	for i := range ir.Jobs {
		j := &ir.Jobs[i]
		if j.Schedule == "" {
			continue
		}
		if _, err := parseCron(j.Schedule); err != nil {
			logger.Error("declared job schedule has an unparseable cron spec and is not synced",
				"job", j.ID(), "spec", j.Schedule, "err", err)
			continue
		}

		queue := j.Queue
		if queue == "" {
			queue = "default"
		}
		// Timeout shapes mirror SubmitJob: none → NULL, undeclared → the
		// 30-minute default.
		var timeoutMS any
		switch {
		case j.TimeoutNone:
			timeoutMS = nil
		case j.TimeoutMS > 0:
			timeoutMS = j.TimeoutMS
		default:
			timeoutMS = 30 * 60 * 1000
		}
		owner := j.VisibleTo
		if owner == "*" {
			owner = ""
		}

		// The conflict update touches only rows this sync owns. A name
		// collision with a built-in's row leaves the built-in's queue
		// and timeout in place — and the collision is loud below, since
		// the declared clause then never syncs.
		tag, err := pool.Exec(ctx, `
INSERT INTO atlantis.job_schedules
    (job_name, cron_spec, default_args, queue, max_retries, timeout_ms, owner_caller, managed_by)
VALUES ($1, $2, '{}', $3, $4, $5, $6, 'dsl')
ON CONFLICT (job_name) DO UPDATE SET
    cron_spec    = EXCLUDED.cron_spec,
    queue        = EXCLUDED.queue,
    max_retries  = EXCLUDED.max_retries,
    timeout_ms   = EXCLUDED.timeout_ms,
    owner_caller = EXCLUDED.owner_caller
WHERE job_schedules.managed_by = 'dsl'`,
			j.ID(), j.Schedule, queue, j.Retries, timeoutMS, owner)
		if err != nil {
			return fmt.Errorf("sync schedule %s: %w", j.ID(), err)
		}
		if tag.RowsAffected() == 0 {
			logger.Error("declared job schedule collides with a built-in schedule row and is not synced",
				"job", j.ID())
			continue
		}
		keep = append(keep, j.ID())
	}

	if _, err := pool.Exec(ctx, `
DELETE FROM atlantis.job_schedules
 WHERE managed_by = 'dsl' AND NOT (job_name = ANY($1))`, keep); err != nil {
		return fmt.Errorf("prune removed schedules: %w", err)
	}
	return nil
}
