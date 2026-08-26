// Package jobs implements the declarative-job runtime: an in-Postgres
// queue, worker pool, scheduler, and the handler-registration surface
// the typed Go SDK generates code against.
//
// Architecture:
//
//   - Registry holds the runtime map of "namespace.JobName" -> Handler.
//     Registry.Register populates it; the generated SDK calls that from the
//     serving binary's startup wiring. Worker.handleOne resolves a claimed
//     row's job_name through Registry.Lookup and dispatches with the
//     deserialized typed args.
//
//   - Worker is the drain loop, one goroutine per queue. Worker.Run wakes on
//     LISTEN/NOTIFY for atl_jobs or a 1s ticker, and Worker.drainOnce claims a
//     batch with FOR UPDATE SKIP LOCKED, dispatches each job, and marks rows
//     complete or failed in their own transactions. A heartbeat goroutine per
//     claimed job extends claimed_until so a peer cannot poach it mid-work.
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
// The caller-facing types — Handler, Registry, Worker, Config — live in the
// client SDK, github.com/rachitkumar205/atlantis/clients/go/jobs, so a caller
// imports only that. The aliases below re-export them for server-internal use,
// and this package adds the server-only half: sweeper, reaper, scheduler,
// workflows, tracing and remote dispatch.
package jobs

import (
	sdkjobs "github.com/rachitkumar205/atlantis/clients/go/jobs"
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
