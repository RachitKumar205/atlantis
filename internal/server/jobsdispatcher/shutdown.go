// Graceful shutdown for the dispatcher.
//
// Sequence on SIGTERM (driven by cmd/server/main.go between gRPC
// GracefulStop and pgx pool Close):
//
//  1. Cancel queue drain-loop contexts → no new claims, no new
//     dispatches. The drainOnce + sweepAckTimeouts goroutines wind
//     down on the next iteration.
//  2. For each open session: mark drained + send Goodbye. Worker
//     stops getting new dispatches but can finish what's in flight.
//  3. Poll inflight counts. When a session's count hits zero, close
//     its stream cleanly (Recv loop returns, defer unregisters).
//  4. Bounded wait: after ShutdownBudget, force-revoke every still-
//     in-flight row across remaining sessions and release back to
//     pending. The shared SKIP LOCKED claim picks them up on next
//     startup.
//
// The reason text on Revoke and released rows distinguishes
// graceful-drained completion ("shutdown_drained") from forced-
// release ("shutdown_budget_exceeded") so an operator inspecting
// last_error during an incident can tell which path fired.

package jobsdispatcher

import (
	"context"
	"time"

	"github.com/rachitkumar205/atlantis/clients/go/jobs"
)

// Shutdown initiates graceful drain. Blocks until either all sessions
// have idled to zero in-flight, or the configured ShutdownBudget
// elapses (whichever first). Returns the count of rows that had to be
// force-released (>0 indicates the budget was exceeded).
//
// Caller MUST stop the queue drain loops first by canceling the ctx
// passed to RunQueue; this function does not own those goroutines.
//
// Safe to call multiple times; subsequent calls return immediately.
func (d *Dispatcher) Shutdown(ctx context.Context) int {
	d.drainStopOnce.Do(func() { close(d.drainStopCh) })

	// Snapshot sessions; further register/unregister can happen but
	// the drained sessions won't accept new dispatches.
	d.mu.RLock()
	sessions := make([]*session, 0, len(d.sessions))
	for _, s := range d.sessions {
		sessions = append(sessions, s)
	}
	d.mu.RUnlock()

	for _, s := range sessions {
		s.markDrained()
		// Best-effort Goodbye push. If the outbox is wedged, we'll
		// force-close in step 4 anyway.
		select {
		case s.outbox <- &DispatchEnvelope{Goodbye: &Goodbye{Reason: "server_shutdown"}}:
		default:
		}
		s.appendEvent(sessionEvent{
			At: time.Now(), Kind: "drained_started", Note: "server_shutdown",
		})
	}

	deadline := time.Now().Add(d.cfg.ShutdownBudget)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		stillInflight := 0
		for _, s := range sessions {
			stillInflight += s.inflightCount()
		}
		if stillInflight == 0 {
			d.cfg.Logger.Info("dispatcher: graceful shutdown drained cleanly",
				"sessions", len(sessions))
			return 0
		}
		if time.Now().After(deadline) {
			released := d.forceReleaseAll(ctx, sessions)
			d.cfg.Logger.Warn("dispatcher: shutdown budget exceeded, force-released rows",
				"sessions", len(sessions), "released", released)
			return released
		}
		select {
		case <-ctx.Done():
			released := d.forceReleaseAll(ctx, sessions)
			return released
		case <-tick.C:
		}
	}
}

// forceReleaseAll iterates every remaining session and releases each
// in-flight row back to pending. Used when the ShutdownBudget elapses
// before workers naturally drain.
func (d *Dispatcher) forceReleaseAll(ctx context.Context, sessions []*session) int {
	released := 0
	releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range sessions {
		inflight := s.snapshotInflight()
		for _, row := range inflight {
			// Best-effort Revoke push. Worker may or may not see it.
			select {
			case s.outbox <- &DispatchEnvelope{Revoke: &Revoke{
				JobID: row.jobID, Reason: "shutdown_budget_exceeded",
			}}:
			default:
			}
			if err := jobs.ReleaseRow(releaseCtx, d.pool, row.jobID, s.claimedBy(), "shutdown_budget_exceeded"); err != nil {
				d.cfg.Logger.Warn("dispatcher: release on shutdown timeout",
					"session", s.id, "row", row.jobID, "err", err)
				continue
			}
			released++
			revokedTotal.WithLabelValues(s.queue, "shutdown_budget_exceeded").Inc()
		}
		s.close()
	}
	_ = ctx
	return released
}
