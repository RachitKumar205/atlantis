// Authorization for dispatched workers. A worker can only receive
// jobs whose `visible_to` declaration permits its caller identity.
//
// The visible_to gate mirrors the existing SubmitJob authz (in
// internal/server/admin/jobs.go). SubmitJob asks "may this caller
// submit?"; the dispatcher asks "may this worker handle?". Same
// field, opposite direction. Permissive default: a job without an
// explicit visible_to is handleable by any authenticated worker.
//
// Checked at two points:
//
//  1. At Open: every job name in the worker's declared JobNames list
//     must be in scope, so a worker never enters the dispatch rotation
//     for jobs it cannot handle.
//
//  2. At dispatch, before a claimed row is pushed. Between Open and
//     dispatch the IR can be re-applied with a tighter visible_to that
//     revokes this worker, and without the re-check dispatch continues
//     to it until the next reconnect.

package jobsdispatcher

import (
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// CheckWorkerAuthz verifies the caller CN (with its configured
// aliases) is authorized to handle every job in jobNames. Returns a
// gRPC status error on the first mismatch so the streaming handler
// can `return err` to close the stream with the right code.
//
// callerCN is the cert CN, "anonymous" in insecure dev mode. aliases is the
// operator-configured alias set for that caller from caller_identities.aliases;
// an empty slice matches on the CN alone.
//
// Dev mode does not auto-allow everything for anonymous, which would let a
// mis-configured production deploy bypass authz. A dev-mode worker's jobs
// declare `visible_to "anonymous"` or `visible_to "*"`.
func CheckWorkerAuthz(callerCN string, aliases []string, jobNames []string, ir *dsl.IR) error {
	if ir == nil {
		return status.Error(codes.FailedPrecondition, "no IR loaded; cannot authorize workers")
	}
	for _, name := range jobNames {
		job := lookupJob(ir, name)
		if job == nil {
			return status.Errorf(codes.NotFound, "unknown job %q", name)
		}
		if !jobVisibleTo(job, callerCN, aliases) {
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
func CheckSingleAuthz(callerCN string, aliases []string, jobName string, ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("no IR loaded")
	}
	job := lookupJob(ir, jobName)
	if job == nil {
		return fmt.Errorf("unknown job %q", jobName)
	}
	if !jobVisibleTo(job, callerCN, aliases) {
		return fmt.Errorf("caller %q not authorized for job %q", callerCN, jobName)
	}
	return nil
}

// lookupJob finds a Job by its canonical "namespace.Name" id. The IR
// keeps jobs in a sorted slice (see internal/dsl/ir.go's Lower path), and a
// linear scan suits the tens-per-IR job count. Thousands would need a map on
// *dsl.IR.
func lookupJob(ir *dsl.IR, id string) *dsl.Job {
	for i := range ir.Jobs {
		if ir.Jobs[i].ID() == id {
			return &ir.Jobs[i]
		}
	}
	return nil
}

// VisibleToMatches decides whether a caller satisfies a `visible_to` value.
//
// Empty or "*" means any caller; otherwise the value must equal the caller's
// CN or one of its configured aliases. Migration 0017 states those three rules
// as the semantics of the feature, and they are written once here because two
// gates need them.
//
// Exported so SubmitJob shares it. A second copy implementing only the first
// two rules lets a caller renamed from `vendor` to `vendor-v2` with
// aliases={'vendor'} still handle a job declared `visible_to "vendor"` while no
// longer being able to enqueue it, so the two gates disagree about who a caller
// is and each looks correct alone.
func VisibleToMatches(visibleTo, callerCN string, aliases []string) bool {
	if visibleTo == "" || visibleTo == "*" {
		return true
	}
	if visibleTo == callerCN {
		return true
	}
	for _, a := range aliases {
		if visibleTo == a {
			return true
		}
	}
	return false
}

// jobVisibleTo is the dsl.Job-shaped form of VisibleToMatches.
func jobVisibleTo(job *dsl.Job, callerCN string, aliases []string) bool {
	return VisibleToMatches(job.VisibleTo, callerCN, aliases)
}
