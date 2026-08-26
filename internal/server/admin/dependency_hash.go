package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// callerDependencyHash reduces the checkpoint to the part of it that decides
// what this caller's next apply will do, and hashes that.
//
// The apply must refuse if the state the caller planned against moved. Hashing
// atlantis.ir_checkpoint, one row holding every caller's schema merged
// together, makes that promise "nothing anywhere changed", so any caller's
// apply invalidates every other caller's outstanding plan and two teams
// deploying on the same afternoon re-plan each other over schemas that never
// touched.
//
// Once a plan can be approved by a human, that token expires the approval for
// reasons the approver cannot see.
//
// It hashes the union of three sets:
//
//   - Members the caller owns. Its own schema is the from-side of its diff.
//   - Members it reads — entities its fields reference, entities its queries
//     and procedures touch, jobs its workflows run. If one of those moved, the
//     plan was computed against a stale picture of it.
//   - Members this apply will emit DDL for. The emitter works from the whole
//     merged diff, not the submitter's slice of it, so anything in the diff is
//     something this apply is about to act on. Normally that is only the
//     submitter's own work, because a caller's registrations and the
//     checkpoint are written in one transaction and cannot disagree. Rollback
//     breaks that: it rewinds the checkpoint and leaves every caller's
//     registrations where they were, so the next apply — by anybody — re-emits
//     what the rollback removed. Including the diff means that apply is
//     refused rather than silently undoing the rollback.
//
// A member is dropped only when it can be shown to belong to another caller and
// to be none of the three. Attribution comes from the "caller:path" prefix the
// parser stamps onto every source path, and for entities, which carry no source
// path in the IR, from the same ownership map the differ was given, so the hash
// and the classification cannot disagree about ownership. An unattributable
// member stays in the hash, costing a re-plan rather than admitting an apply
// that should have been refused.
func callerDependencyHash(caller string, prior *dsl.IR, ownership map[string]string,
	callerFiles []*dsl.File, d *codegen.Diff) (string, error) {
	// No checkpoint means no world to have moved. Returning "" here matches
	// what the callers do with it: an empty token disables the comparison,
	// which is correct for the first apply into an empty database.
	if prior == nil {
		return "", nil
	}

	deps := callerDependencies(callerFiles)
	changed := map[string]bool{}
	for _, ch := range d.All() {
		changed[ch.EntityID] = true
	}

	keep := func(id, sourcePath string) bool {
		if deps[id] || changed[id] {
			return true
		}
		owner, known := checkpointMemberOwner(id, sourcePath, ownership)
		return !known || owner == caller
	}

	view := &dsl.IR{Version: prior.Version}
	for _, e := range prior.Entities {
		if keep(e.ID(), "") {
			view.Entities = append(view.Entities, e)
		}
	}
	for _, q := range prior.Queries {
		if keep(q.ID(), q.SourcePath) {
			view.Queries = append(view.Queries, q)
		}
	}
	for _, p := range prior.Procedures {
		if keep(p.ID(), p.SourcePath) {
			view.Procedures = append(view.Procedures, p)
		}
	}
	for _, j := range prior.Jobs {
		if keep(j.ID(), j.SourcePath) {
			view.Jobs = append(view.Jobs, j)
		}
	}
	for _, w := range prior.Workflows {
		if keep(w.ID(), w.SourcePath) {
			view.Workflows = append(view.Workflows, w)
		}
	}
	for _, e := range prior.Ephemerals {
		if keep(e.ID(), e.SourcePath) {
			view.Ephemerals = append(view.Ephemerals, e)
		}
	}

	raw, err := view.EncodeJSON()
	if err != nil {
		// Propagated rather than swallowed into an empty string. An empty
		// token reads as "no checkpoint" everywhere it is used, so swallowing
		// this would turn an encoding failure into a silently disabled
		// staleness check.
		return "", fmt.Errorf("encode dependency view: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// checkpointMemberOwner names the caller a checkpoint member came from, and
// reports whether it could be established at all.
//
// Source paths are authoritative because dsl.Parse is always handed
// "<caller>:<path>" by parseSubmitted and loadOtherCallers, and lowering
// copies that through to every member that records one. Entities record none,
// so they fall back to the ownership map.
//
// The "could not establish it" answer is a backstop rather than a live case,
// and it is worth being exact about why. For an entity it never decides
// anything: the ownership map covers precisely the entities the new IR
// contains, so an entity it cannot name is one the checkpoint has and the new
// IR does not — which puts it in the diff, and the diff keeps it first. For
// everything else the source path is always set by current lowering. What
// reaches here is a checkpoint written before a member kind recorded one, and
// what would reach here in future is a member kind added without one. Both
// want the same answer, and it is the conservative one.
func checkpointMemberOwner(id, sourcePath string, ownership map[string]string) (string, bool) {
	if c, _, ok := strings.Cut(sourcePath, ":"); ok && c != "" {
		return c, true
	}
	c, ok := ownership[id]
	return c, ok
}

// callerDependencies returns the set of checkpoint member IDs this caller's
// own files read from elsewhere.
//
// buildCrossCallerRefs does the entity half already — it is the scan the
// differ uses to decide whether a removal breaks somebody — and it is used
// here in the mirror direction: pointed at the submitter's files rather than
// everyone else's, it answers "what does this caller read" instead of "who
// reads this caller". Its output carries both "ns.Entity" and
// "ns.Entity.field" keys; the field-shaped ones never collide with a member ID
// (those hold exactly one dot) and are left in place rather than filtered out.
//
// Workflow steps are the half it does not cover. A step names its job as
// [ns.]Name and resolves across namespaces, so a workflow can run another
// caller's job, and a change to that job's arguments changes what this
// caller's workflow does. The scan is kept here rather than folded into
// buildCrossCallerRefs because that function also feeds diff classification,
// where adding job references would move removals between buckets — a
// behaviour change that has nothing to do with plan staleness.
func callerDependencies(callerFiles []*dsl.File) map[string]bool {
	out := buildCrossCallerRefs(callerFiles)
	for _, f := range callerFiles {
		wf, ok := declWorkflow(f)
		if !ok {
			continue
		}
		for _, w := range wf {
			for _, s := range w.Steps {
				out[workflowJobID(w.Namespace, s.JobRef)] = true
			}
			for _, c := range w.Compensations {
				out[workflowJobID(w.Namespace, c.JobRef)] = true
			}
		}
	}
	return out
}

// declWorkflow collects the workflow declarations in one file.
func declWorkflow(f *dsl.File) ([]*dsl.WorkflowDecl, bool) {
	var out []*dsl.WorkflowDecl
	for _, decl := range f.Decls {
		if w, ok := decl.(*dsl.WorkflowDecl); ok {
			out = append(out, w)
		}
	}
	return out, len(out) > 0
}

// workflowJobID resolves a step's job reference the way lowering does: an
// unqualified name means a job in the workflow's own namespace.
func workflowJobID(workflowNS string, ref dsl.EntityRef) string {
	ns := ref.Namespace
	if ns == "" {
		ns = workflowNS
	}
	return ns + "." + ref.Name
}
