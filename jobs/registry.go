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
//
// Caller-facing types (Handler, Registry, Worker, Config, Checkpoint)
// live in the client SDK (github.com/rachitkumar205/atlantis-go/jobs)
// so callers import only the SDK. This package re-exports them for
// server-internal use and adds server-only code (sweeper, workflows,
// tracing, remote dispatch).
package jobs

import (
	sdkjobs "github.com/rachitkumar205/atlantis-go/jobs"
)

// Re-export caller-facing types from the client SDK so server code
// can import a single package.

type Handler = sdkjobs.Handler
type HandlerFunc = sdkjobs.HandlerFunc
type Registry = sdkjobs.Registry
type Worker = sdkjobs.Worker
type Config = sdkjobs.Config

type HandlerNotRegisteredError = sdkjobs.HandlerNotRegisteredError
type JobCompleteHook = sdkjobs.JobCompleteHook
type TraceHook = sdkjobs.TraceHook

var NewRegistry = sdkjobs.NewRegistry
var NewWorker = sdkjobs.NewWorker
var DefaultConfig = sdkjobs.DefaultConfig
var Checkpoint = sdkjobs.Checkpoint
