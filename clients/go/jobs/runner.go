package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config tunes the worker pool. The defaults are safe on a multi-pod
// deployment with no further tuning.
type Config struct {
	// Schema is defaulted to "atlantis" and read by nothing: sql.go names
	// atlantis.jobs literally in every statement. Setting it changes no query.
	Schema string

	PodID string

	// BatchSize is how many rows one claim takes. Fifty keeps a quiet queue at
	// about one SQL round trip per drain while leaving progress visible.
	BatchSize int

	// DrainInterval is the poll cadence when LISTEN/NOTIFY delivers nothing.
	DrainInterval time.Duration

	// HeartbeatBudget is the claim lease: drainOnce sets claimed_until this far
	// ahead, and a ticker at a third of it extends the lease while a handler
	// runs. A pod that dies frees its rows one budget later, so a shorter one
	// recovers faster and leaves less room for a slow heartbeat.
	HeartbeatBudget time.Duration

	Logger *slog.Logger
}

// DefaultConfig returns a Config with safe defaults populated.
//
// PodID is hostname-pid by default — enough granularity for an
// operator scanning atlantis.jobs to spot which pod owns a stuck
// claim. Override in tests via Config.PodID.
func DefaultConfig() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "atlantis"
	}
	return Config{
		Schema:          "atlantis",
		PodID:           fmt.Sprintf("%s-%d", host, os.Getpid()),
		BatchSize:       50,
		DrainInterval:   time.Second,
		HeartbeatBudget: 2 * time.Minute,
		Logger:          slog.Default(),
	}
}

// JobCompleteHook is called by the worker after a job completes or
// fails terminally. Server-internal code (workflow engine) supplies
// an implementation; caller-side workers leave it nil.
type JobCompleteHook interface {
	OnJobComplete(ctx context.Context, jobID int64)
	OnJobFailed(ctx context.Context, jobID int64, errMsg string)
}

// TraceHook lets the server inject OTel distributed-tracing into the
// worker's dispatch path without pulling OTel into the client SDK.
// When nil, tracing is a no-op — the handler runs under the bare ctx.
type TraceHook interface {
	// ResumeTrace reconstructs a parent span from the serialized
	// trace_ctx column and returns a ctx carrying that parent.
	ResumeTrace(ctx context.Context, traceCtxJSON []byte) context.Context
	// StartSpan creates a child span for the handler dispatch and
	// returns the wrapped ctx plus a finish function.
	StartSpan(ctx context.Context, jobName string) (context.Context, func())
}

// Worker drains atlantis.jobs for a specific queue. Multiple workers
// on the same queue across pods coexist safely: claim uses FOR
// UPDATE SKIP LOCKED, and the per-row lease (claimed_until) lets a
// peer recover work from a crashed pod after the lease expires.
//
// A Worker is bound to one queue name. Run two Workers (in different
// goroutines, same Registry) to drain two queues concurrently — the
// SQL is partitioned by queue so they don't contend.
type Worker struct {
	pool     *pgxpool.Pool
	registry *Registry
	queue    string
	cfg      Config

	completeHook JobCompleteHook
	traceHook    TraceHook

	lastClaimNS atomic.Int64
}

// NewWorker constructs a Worker. The registry must be populated
// before Run is called — if a job arrives whose handler isn't
// registered, the worker reports a transient claim error and the
// row stays pending for the next deploy that has the handler.
func NewWorker(pool *pgxpool.Pool, registry *Registry, queue string, cfg Config) *Worker {
	if cfg.Schema == "" {
		cfg.Schema = "atlantis"
	}
	if cfg.PodID == "" {
		cfg.PodID = DefaultConfig().PodID
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = time.Second
	}
	if cfg.HeartbeatBudget <= 0 {
		cfg.HeartbeatBudget = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Worker{
		pool:     pool,
		registry: registry,
		queue:    queue,
		cfg:      cfg,
	}
	w.lastClaimNS.Store(time.Now().UnixNano())
	return w
}

// SetCompleteHook attaches an optional hook called on job completion
// or terminal failure. The server uses this to wire the workflow
// engine; caller-side workers typically leave it nil.
func (w *Worker) SetCompleteHook(h JobCompleteHook) { w.completeHook = h }

// SetTraceHook attaches an optional tracing hook so the worker
// resumes distributed traces from the submitter and creates child
// spans for handler dispatch. Without a hook, tracing is a no-op.
func (w *Worker) SetTraceHook(h TraceHook) { w.traceHook = h }

// Run blocks until ctx is canceled, draining the queue. Errors
// from individual drain passes are logged; only ctx cancellation
// propagates up. The expected production pattern is to launch
// Run in a goroutine from cmd/server/main.go.
func (w *Worker) Run(ctx context.Context) error {
	// Refuse to start against a server schema older than this SDK. One
	// catalogue query per worker lifetime; see requireOwnerCallerColumn for
	// what the skew does if it is not caught here.
	if err := requireOwnerCallerColumn(ctx, w.pool); err != nil {
		return err
	}

	// Seed-drain pass clears anything that landed before the LISTEN
	// session establishes. The ticker covers steady-state polling
	// for cases where LISTEN's notification got dropped (network
	// blip, pod restart) or where the trigger fired before this pod
	// was subscribing.
	w.drainOnce(ctx)

	ticker := time.NewTicker(w.cfg.DrainInterval)
	defer ticker.Stop()

	notifyCh := make(chan struct{}, 1)
	listenCtx, cancelListen := context.WithCancel(ctx)
	defer cancelListen()
	go w.runListenLoop(listenCtx, notifyCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.drainOnce(ctx)
		case <-notifyCh:
			w.drainOnce(ctx)
		}
	}
}

// LastClaimAt is the wall-clock time of the most recent successful
// claim. Read by /readyz so an operator can spot a worker that's
// fallen asleep (stuck LISTEN connection, deadlocked drain loop)
// well before the queue itself reports lag.
func (w *Worker) LastClaimAt() time.Time {
	return time.Unix(0, w.lastClaimNS.Load())
}

// runListenLoop delegates to PgListen, filtering atl_jobs notifications
// to this worker's queue. PgListen owns the reconnect cadence + panic
// recovery; the closure adapts the queue-match predicate.
func (w *Worker) runListenLoop(ctx context.Context, notifyCh chan struct{}) {
	PgListen(ctx, w.pool, "atl_jobs", func(payload string) bool {
		return payload == w.queue
	}, notifyCh, w.cfg.Logger)
}

// drainOnce claims a batch and processes each row. Errors from
// individual rows don't abort the batch; the worker logs and moves
// on. A claim batch is a single SQL round-trip; per-row processing
// runs sequentially within the goroutine because handlers can be
// expensive and a stuck handler shouldn't starve sibling handlers.
//
// For concurrent per-job processing, run multiple Workers on the
// same queue — they coordinate via SKIP LOCKED.
func (w *Worker) drainOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			w.cfg.Logger.Error("jobs drainOnce panic", "queue", w.queue, "panic", rec)
		}
	}()
	leaseDeadline := time.Now().Add(w.cfg.HeartbeatBudget)
	rows, err := ClaimRows(ctx, w.pool, w.queue, nil, w.cfg.BatchSize,
		w.cfg.PodID, leaseDeadline, WorkerKindDirectPG, "")
	if err != nil {
		w.cfg.Logger.Warn("jobs claim", "queue", w.queue, "err", err)
		return
	}
	if len(rows) > 0 {
		w.lastClaimNS.Store(time.Now().UnixNano())
	}
	for _, r := range rows {
		w.handleOne(ctx, r)
	}
}

// handleOne dispatches one claimed row through the registry, under its own
// timeout_ms deadline so a hung handler cannot freeze the drainer.
//
// Three outcomes:
//
//   - No handler registered. reportTransientFailure bumps attempts and leaves
//     status='running' until the lease expires, so a peer that has the handler
//     can claim it.
//   - Handler returns nil. Marked complete in its own transaction.
//   - Handler returns an error. attempts is bumped; past max_retries the row
//     moves to atlantis.jobs_dead, otherwise it returns to 'pending' and the
//     next drain pass retries it. buildClaimSQL gates on scheduled_for, so a
//     retry is immediate unless something moved that column.
//
// A handler running longer than HeartbeatBudget extends its lease through the
// checkpoint API. Without that wiring, a timeout longer than HeartbeatBudget
// lets a peer claim the row mid-work.
func (w *Worker) handleOne(ctx context.Context, r ClaimedRow) {
	handler := w.registry.Lookup(r.JobName)
	if handler == nil {
		err := &HandlerNotRegisteredError{JobID: r.JobName}
		w.cfg.Logger.Warn("jobs handler missing", "queue", w.queue, "job_id", r.JobName, "row", r.ID)
		w.reportTransientFailure(ctx, r, err)
		return
	}

	// Resume the distributed trace from the submitter (if present)
	// and start a worker-side span so the handler's work appears as
	// a child of the submit call. When no TraceHook is installed
	// (common for caller-side workers), tracing is a no-op.
	runCtx := ctx
	endSpan := func() {}
	if w.traceHook != nil {
		runCtx = w.traceHook.ResumeTrace(runCtx, r.TraceCtx)
		runCtx, endSpan = w.traceHook.StartSpan(runCtx, r.JobName)
	}
	defer endSpan()

	if r.TimeoutMS > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, time.Duration(r.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	runCtx = withCheckpointer(runCtx, newCheckpointer(w.pool, r.ID))

	// Lease-extension heartbeat. A goroutine ticks every
	// HeartbeatBudget/3 to bump claimed_until so a peer doesn't
	// poach the row mid-work. The done channel terminates the
	// heartbeat when handler returns.
	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		ticker := time.NewTicker(w.cfg.HeartbeatBudget / 3)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := ExtendLease(ctx, w.pool, []int64{r.ID}, w.cfg.PodID, w.cfg.HeartbeatBudget); err != nil {
					w.cfg.Logger.Warn("jobs heartbeat", "row", r.ID, "err", err)
				}
			}
		}
	}()

	err := handler.Handle(runCtx, r.Args)
	stopHB()
	hbWG.Wait()

	if err == nil {
		if cerr := MarkComplete(ctx, w.pool, r.ID); cerr != nil {
			w.cfg.Logger.Warn("jobs markComplete", "row", r.ID, "err", cerr)
		}
		if w.completeHook != nil {
			w.completeHook.OnJobComplete(ctx, r.ID)
		}
		return
	}
	w.reportFailure(ctx, r, err)
}

// reportFailure delegates to the package-level ReportFailure helper.
// On terminal failure the completion hook fires; the helper itself
// doesn't know about hooks (kept SQL-only).
func (w *Worker) reportFailure(ctx context.Context, r ClaimedRow, handlerErr error) {
	msg := handlerErr.Error()
	terminal := r.Attempts >= r.MaxRetries
	if err := ReportFailure(ctx, w.pool, r.ID, r.Attempts, r.MaxRetries, msg); err != nil {
		w.cfg.Logger.Warn("jobs reportFailure", "row", r.ID, "err", err)
	}
	if terminal && w.completeHook != nil {
		w.completeHook.OnJobFailed(ctx, r.ID, msg)
	}
}

// reportTransientFailure handles errors fixable at runtime with no code change,
// which today is the missing-handler case alone.
//
// It bumps attempts, so a handler that stays missing reaches the DLQ, and
// leaves the row in `running` until the lease expires, so a peer pod that has
// the handler can claim it.
func (w *Worker) reportTransientFailure(ctx context.Context, r ClaimedRow, err error) {
	if r.Attempts >= r.MaxRetries {
		_ = MoveToDLQ(ctx, w.pool, r.ID, err.Error())
		return
	}
	_, qerr := w.pool.Exec(ctx, `
UPDATE atlantis.jobs
   SET last_error    = $1,
       last_error_at = now()
 WHERE id = $2`, err.Error(), r.ID)
	if qerr != nil {
		w.cfg.Logger.Warn("jobs reportTransientFailure", "row", r.ID, "err", qerr)
	}
}
