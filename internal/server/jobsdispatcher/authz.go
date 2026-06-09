// Authorization for dispatched workers. A worker can only receive
// jobs whose `visible_to` declaration permits its caller identity.
//
// The visible_to gate mirrors the existing SubmitJob authz (in
// internal/server/admin/jobs.go). SubmitJob asks "may this caller
// submit?"; the dispatcher asks "may this worker handle?". Same
// field, opposite direction. Permissive default: a job without an
// explicit visible_to is handleable by any authenticated worker.
//
// We check at two points:
//
//  1. At Open: every job name in the worker's declared JobNames list
//     must be in scope. Rejecting at Open prevents a worker from ever
//     entering the dispatch rotation for jobs it can't handle.
//
//  2. At dispatch: when we're about to push a claimed row, re-check.
//     Defense-in-depth — between Open and dispatch the IR could have
//     been re-applied with a tighter visible_to that revokes this
//     worker. Without the re-check we'd silently keep dispatching
//     to a now-unauthorized worker until the next reconnect.

package jobsdispatcher

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// CheckWorkerAuthz verifies the caller CN is authorized to handle
// every job in jobNames. Returns a gRPC status error on the first
// mismatch so the streaming handler can `return err` to close the
// stream with the right code.
//
// callerCN is the cert CN ("anonymous" in insecure dev mode). When
// dev mode is enabled, the caller still has to declare which jobs
// they handle — we don't auto-allow everything for anonymous, since
// that would let a misconfigured prod deploy silently bypass authz.
// Operators wanting dev-mode workers can configure their jobs with
// `visible_to "anonymous"` or `visible_to "*"`.
func CheckWorkerAuthz(callerCN string, jobNames []string, ir *dsl.IR) error {
	if ir == nil {
		return status.Error(codes.FailedPrecondition, "no IR loaded; cannot authorize workers")
	}
	for _, name := range jobNames {
		job := lookupJob(ir, name)
		if job == nil {
			return status.Errorf(codes.NotFound, "unknown job %q", name)
		}
		if !jobVisibleTo(job, callerCN) {
			return status.Errorf(codes.PermissionDenied,
				"caller %q not authorized for job %q", callerCN, name)
		}
	}
	return nil
}

// CheckSingleAuthz is the dispatch-time defense-in-depth variant.
// One job, returns a plain error so the dispatcher can log it as
// `authz_rejected_post_open` and release the row instead of failing
// the whole session.
func CheckSingleAuthz(callerCN string, jobName string, ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("no IR loaded")
	}
	job := lookupJob(ir, jobName)
	if job == nil {
		return fmt.Errorf("unknown job %q", jobName)
	}
	if !jobVisibleTo(job, callerCN) {
		return fmt.Errorf("caller %q not authorized for job %q", callerCN, jobName)
	}
	return nil
}

// lookupJob finds a Job by its canonical "namespace.Name" id. The IR
// keeps jobs in a sorted slice (see internal/dsl/ir.go's Lower path);
// linear scan is fine for the typical job-count of tens-per-IR. If
// future schemas push this into the thousands we'll add a built-in
// map to *dsl.IR.
func lookupJob(ir *dsl.IR, id string) *dsl.Job {
	for i := range ir.Jobs {
		if ir.Jobs[i].ID() == id {
			return &ir.Jobs[i]
		}
	}
	return nil
}

// jobVisibleTo applies the same permissive default that SubmitJob
// uses: empty or "*" means any caller, otherwise the field must
// match the caller CN exactly. Mirroring the existing SubmitJob
// gate (admin/jobs.go) keeps the policy in one mental model.
func jobVisibleTo(job *dsl.Job, callerCN string) bool {
	v := job.VisibleTo
	if v == "" || v == "*" {
		return true
	}
	return v == callerCN
}
