// Package jobs implements the declarative-job runtime: an in-Postgres
// queue, worker pool, scheduler, and the handler-registration surface
// the typed Go SDK generates code against.
//
// Architecture:
//
//   - Registry holds the runtime map of "namespace.JobName" -> Handler.
//     atlantis-server's startup wiring (cmd/server/main.go) populates
//     it via RegisterJobHandlers, which the generated SDK calls.
//     Worker.invoke() looks up the handler by job_name when it claims
//     a row and dispatches with the deserialized typed args.
//
//   - Runner is the drain loop. One goroutine per queue. Wakes on
//     LISTEN/NOTIFY for atl_jobs or a 1s ticker. Claims a batch via
//     FOR UPDATE SKIP LOCKED, dispatches each job through the
//     registry, marks rows complete/failed in their own transactions.
//     A heartbeat goroutine per claimed job extends claimed_until so
//     a peer doesn't poach the row mid-work.
//
//   - Scheduler is a singleton goroutine (elected via
//     pg_try_advisory_lock) that evaluates atlantis.job_schedules
//     rows on a ticker and INSERTs into atlantis.jobs when a fire is
//     due.
//
// The package is decoupled from gRPC so it can be tested in
// isolation; internal/server/admin/jobs.go wraps these primitives in
// the admin RPC surface.
package jobs

import (
	"context"
	"fmt"
	"sync"
)

// Handler is the contract a job's typed handler implements. The
// generated SDK emits per-job interfaces that embed Handler and add
// a strongly-typed Args parameter; under the hood the generated stub
// deserializes args JSON, invokes the typed method, and returns the
// typed handler's error verbatim.
//
// Args is JSON-encoded so the registry stays type-erased — the
// runtime doesn't need to know the per-job struct shape. The
// generated typed wrapper is responsible for json.Unmarshal into the
// caller's args struct before dispatching.
//
// Returning a non-nil error tells the worker to retry per the job's
// max_retries; a nil return marks the job complete.
type Handler interface {
	Handle(ctx context.Context, argsJSON []byte) error
}

// HandlerFunc adapts a plain function to Handler. Useful for tests and
// for caller code that doesn't want to define a type.
type HandlerFunc func(ctx context.Context, argsJSON []byte) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, argsJSON []byte) error {
	return f(ctx, argsJSON)
}

// Registry maps canonical job ids ("namespace.JobName") to handlers.
//
// Concurrent registration is safe but rare — the expected pattern is
// "register everything during server startup, then read-only at
// runtime." The mutex covers the rare cross-goroutine registration
// case (e.g. a test that hot-reloads handlers) and is uncontended on
// the happy path.
//
// The runtime mismatch case (claim a row whose job_name has no
// registered handler) is treated as a transient error: the row's
// attempt counter bumps, last_error notes the missing handler, and
// the row stays pending. This lets a rolling deploy where pod A has
// the handler and pod B doesn't still make progress instead of
// dead-lettering perfectly-good work.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty Registry. Callers populate it via
// Register; the runner reads from it via Lookup.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register binds a Handler to a canonical job id. Re-registering the
// same id replaces the prior handler — convenient for tests but the
// expected production pattern is "register once at startup."
func (r *Registry) Register(jobID string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[jobID] = h
}

// Lookup returns the registered handler for jobID, or nil if none.
// The runner translates a nil return into a transient claim-failure
// (see the comment on Registry).
func (r *Registry) Lookup(jobID string) Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[jobID]
}

// RegisteredIDs returns a stable list of every registered job id.
// Used by the metrics emitter to seed gauges with zero values so a
// missing job_name shows up as 0 instead of being absent from the
// /metrics scrape.
func (r *Registry) RegisteredIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.handlers))
	for id := range r.handlers {
		out = append(out, id)
	}
	return out
}

// HandlerNotRegisteredError is the sentinel the runner reports when a
// claim finds no matching handler. The runner treats this as
// transient (retry, don't DLQ) so a half-deployed cluster makes
// progress on whatever IS handled.
type HandlerNotRegisteredError struct {
	JobID string
}

func (e *HandlerNotRegisteredError) Error() string {
	return fmt.Sprintf("jobs: no handler registered for %s", e.JobID)
}
