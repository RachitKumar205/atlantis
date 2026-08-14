package admin

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// These pin what callerDependencyHash includes and excludes. The end-to-end
// consequence — a plan surviving an unrelated caller's apply — is in
// dependency_hash_pg_test.go; this file is where the boundary itself is drawn,
// because a token that is too narrow and a token that is too wide both look
// like a working system right up until they do not.

// depFile parses one .atl source under the "<caller>:<path>" convention
// parseSubmitted and loadOtherCallers both use. Source paths carry that prefix
// into the checkpoint, and attribution reads it back out, so a test that
// skipped it would exercise a different code path than production.
func depFile(t *testing.T, caller, path, src string) *dsl.File {
	t.Helper()
	f, err := dsl.Parse(caller+":"+path, []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

func depLower(t *testing.T, files ...*dsl.File) *dsl.IR {
	t.Helper()
	ir, err := dsl.Lower(files)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return ir
}

// depToken computes the token the way PlanSchema and ApplyMigration do,
// including the ownership map and the diff, so the tests below cannot pass
// against a composition production never performs.
func depToken(t *testing.T, caller string, callerFiles, otherFiles []*dsl.File, prior *dsl.IR) string {
	t.Helper()
	all := append(append([]*dsl.File{}, callerFiles...), otherFiles...)
	newIR := depLower(t, all...)
	codegen.AssignProtoNumbers(prior, newIR)
	ownership := buildEntityOwnership(caller, callerFiles, otherFiles)
	d := codegen.ComputeDiff(prior, newIR,
		codegen.WithCallerContext(caller, ownership, buildCrossCallerRefs(otherFiles)))
	h, err := callerDependencyHash(caller, prior, ownership, callerFiles, d)
	if err != nil {
		t.Fatalf("callerDependencyHash: %v", err)
	}
	return h
}

const alphaOrderV1 = `
entity Order in alpha {
  id    bigint primary
  total numeric(10, 2) not null
}
`

const alphaOrderV2 = `
entity Order in alpha {
  id    bigint primary
  total numeric(10, 2) not null
  note  text
}
`

const betaGadgetV1 = `
entity Gadget in beta {
  id   bigint primary
  name text
}
`

const betaGadgetV2 = `
entity Gadget in beta {
  id     bigint primary
  name   text
  colour text
}
`

// The property the whole change exists for.
func TestAnotherCallersChangeDoesNotMoveTheToken(t *testing.T) {
	alphaV1 := depFile(t, "alpha", "order.atl", alphaOrderV1)
	alphaV2 := depFile(t, "alpha", "order.atl", alphaOrderV2)
	betaV1 := depFile(t, "beta", "gadget.atl", betaGadgetV1)
	betaV2 := depFile(t, "beta", "gadget.atl", betaGadgetV2)

	// alpha plans its own column against a checkpoint holding beta's v1.
	atPlan := depToken(t, "alpha",
		[]*dsl.File{alphaV2}, []*dsl.File{betaV1}, depLower(t, alphaV1, betaV1))

	// beta applies a column of its own. Nothing alpha declares, references or
	// is about to emit DDL for has moved.
	atApply := depToken(t, "alpha",
		[]*dsl.File{alphaV2}, []*dsl.File{betaV2}, depLower(t, alphaV1, betaV2))

	if atPlan != atApply {
		t.Errorf("beta's unrelated column moved alpha's token (%s -> %s).\n"+
			"That is the failure this scoping exists to remove: two callers with "+
			"no shared schema re-planning each other every deploy.",
			atPlan[:12], atApply[:12])
	}
}

// The counterweight. A token narrowed until it stops protecting is worse than
// the global one it replaced, so the same construction must still refuse when
// the thing that moved is something alpha reads.
func TestADependencyMovingMovesTheToken(t *testing.T) {
	const betaThingV1 = `
entity Thing in beta {
  id    varchar(8) primary
  label text
}
`
	const betaThingV2 = `
entity Thing in beta {
  id     varchar(8) primary
  label  text
  weight int
}
`
	const alphaRefsThing = `
entity Order in alpha {
  id       bigint primary
  thing_id varchar(8) not null references beta.Thing.id
}
`
	alphaF := depFile(t, "alpha", "order.atl", alphaRefsThing)
	betaV1 := depFile(t, "beta", "thing.atl", betaThingV1)
	betaV2 := depFile(t, "beta", "thing.atl", betaThingV2)

	atPlan := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV1}, depLower(t, alphaF, betaV1))
	atApply := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV2}, depLower(t, alphaF, betaV2))

	if atPlan == atApply {
		t.Errorf("beta changed an entity alpha holds a foreign key into and "+
			"alpha's token did not move (both %s). The plan was computed against "+
			"a picture of beta.Thing that no longer exists.", atPlan[:12])
	}
}

// A caller's own edit still moves its own token: two of its deploys racing is
// the case the compare-and-swap was written for in the first place.
func TestTheCallersOwnChangeMovesItsToken(t *testing.T) {
	alphaV1 := depFile(t, "alpha", "order.atl", alphaOrderV1)
	alphaV2 := depFile(t, "alpha", "order.atl", alphaOrderV2)
	betaV1 := depFile(t, "beta", "gadget.atl", betaGadgetV1)

	before := depToken(t, "alpha",
		[]*dsl.File{alphaV2}, []*dsl.File{betaV1}, depLower(t, alphaV1, betaV1))
	after := depToken(t, "alpha",
		[]*dsl.File{alphaV2}, []*dsl.File{betaV1}, depLower(t, alphaV2, betaV1))

	if before == after {
		t.Errorf("alpha's own column landing in the checkpoint left its token at "+
			"%s. A second deploy of the same plan would then re-apply it.", before[:12])
	}
}

// The rollback case, and the reason the diff is part of the scope.
//
// A rollback rewinds the checkpoint and leaves every caller's registrations
// where they were, so the two disagree — the only way that happens. From then
// on the merged diff contains another caller's rolled-back work, and whoever
// applies next re-emits it. Scoping the token to declarations and references
// alone would let alpha walk into that with a plan made before the rollback;
// including whatever is in the diff refuses it instead.
func TestAnotherCallersEntityInTheDiffStaysInTheToken(t *testing.T) {
	alphaF := depFile(t, "alpha", "order.atl", alphaOrderV1)
	betaV2 := depFile(t, "beta", "gadget.atl", betaGadgetV2)

	// Planned while the checkpoint agreed with beta's registrations.
	atPlan := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV2}, depLower(t, alphaF, betaV2))

	// A rollback then rewound the checkpoint to beta's v1. beta's registration
	// still says v2, so beta.Gadget.colour is now in alpha's diff.
	betaV1 := depFile(t, "beta", "gadget.atl", betaGadgetV1)
	atApply := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV2}, depLower(t, alphaF, betaV1))

	if atPlan == atApply {
		t.Errorf("a rollback of beta's schema left alpha's token at %s, so alpha's "+
			"apply would proceed and silently re-emit what the rollback removed.",
			atPlan[:12])
	}
}

// Attribution is the only thing that licenses dropping a member, so a member
// nobody can attribute has to stay.
//
// Reaching that arm takes some setting up, and the first version of this test
// did not reach it: it used an entity the checkpoint held and no file declared,
// which is unattributable but is also a removal, so the diff kept it and
// inverting the attribution rule changed nothing. What actually reaches it is a
// member whose source path was never recorded — a checkpoint written before
// its kind carried one. Built here by blanking the path after the diff is
// computed, so the diff is empty and attribution is the only thing deciding.
func TestAMemberWithNoRecordedSourceStaysInTheToken(t *testing.T) {
	const betaQuery = `
entity Gadget in beta {
  id   bigint primary
  name text
}
query FindByName for Gadget {
  input  { name: text }
  output as Gadget
  sql touches(Gadget) {
    SELECT * FROM atlantis.beta_gadget WHERE name = $name
  }
}
`
	alphaF := depFile(t, "alpha", "order.atl", alphaOrderV1)
	betaF := depFile(t, "beta", "gadget.atl", betaQuery)

	token := func(mutate func(*dsl.CustomQuery)) string {
		t.Helper()
		prior := depLower(t, alphaF, betaF)
		newIR := depLower(t, alphaF, betaF)
		codegen.AssignProtoNumbers(prior, newIR)
		ownership := buildEntityOwnership("alpha", []*dsl.File{alphaF}, []*dsl.File{betaF})
		d := codegen.ComputeDiff(prior, newIR,
			codegen.WithCallerContext("alpha", ownership, buildCrossCallerRefs([]*dsl.File{betaF})))
		if !d.IsEmpty() {
			t.Fatalf("the diff should be empty here, or it and not attribution is "+
				"what keeps the query: %+v", d.All())
		}
		if len(prior.Queries) != 1 {
			t.Fatalf("expected one query in the checkpoint, got %d", len(prior.Queries))
		}
		prior.Queries[0].SourcePath = ""
		mutate(&prior.Queries[0])
		h, err := callerDependencyHash("alpha", prior, ownership, []*dsl.File{alphaF}, d)
		if err != nil {
			t.Fatalf("callerDependencyHash: %v", err)
		}
		return h
	}

	before := token(func(*dsl.CustomQuery) {})
	after := token(func(q *dsl.CustomQuery) { q.SQL += " LIMIT 1" })

	if before == after {
		t.Errorf("a checkpoint member with no recorded source was dropped from the "+
			"token (both %s). Dropping requires proof of another owner, and an "+
			"absent source path is not proof of anything.", before[:12])
	}
}

// buildCrossCallerRefs sees foreign keys and query/procedure touches. It does
// not see workflow steps, which name a job as [ns.]Name and resolve across
// namespaces — so a workflow can run another caller's job, and callerDependencies
// has to add that edge itself.
func TestAJobAWorkflowRunsStaysInTheToken(t *testing.T) {
	const betaJobV1 = `
job Import in beta {
  args { vendor_id varchar(7) not null }
}
`
	const betaJobV2 = `
job Import in beta {
  args { vendor_id varchar(7) not null }
  retries 5
}
`
	const alphaWorkflow = `
workflow Onboard in alpha {
  state { vendor_id varchar(7) not null }
  step run {
    job beta.Import
    args { vendor_id: $vendor_id }
  }
}
`
	alphaF := depFile(t, "alpha", "onboard.atl", alphaWorkflow)
	betaV1 := depFile(t, "beta", "jobs.atl", betaJobV1)
	betaV2 := depFile(t, "beta", "jobs.atl", betaJobV2)

	atPlan := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV1}, depLower(t, alphaF, betaV1))
	atApply := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV2}, depLower(t, alphaF, betaV2))

	if atPlan == atApply {
		t.Errorf("beta changed the retry budget of a job alpha's workflow runs and "+
			"alpha's token did not move (both %s).", atPlan[:12])
	}
}

// The mirror of the test above: beta's OTHER job, the one nothing of alpha's
// runs, must not hold alpha's plan hostage. Without this the workflow edge
// could be "implemented" by keeping every job, which passes the test above and
// gives back the global token for anyone who declares one.
func TestAJobNoWorkflowOfOursRunsIsDroppedFromTheToken(t *testing.T) {
	const betaJobsV1 = `
job Import in beta {
  args { vendor_id varchar(7) not null }
}
job Sweep in beta {
  args { batch varchar(7) not null }
}
`
	const betaJobsV2 = `
job Import in beta {
  args { vendor_id varchar(7) not null }
}
job Sweep in beta {
  args { batch varchar(7) not null }
  retries 9
}
`
	const alphaWorkflow = `
workflow Onboard in alpha {
  state { vendor_id varchar(7) not null }
  step run {
    job beta.Import
    args { vendor_id: $vendor_id }
  }
}
`
	alphaF := depFile(t, "alpha", "onboard.atl", alphaWorkflow)
	betaV1 := depFile(t, "beta", "jobs.atl", betaJobsV1)
	betaV2 := depFile(t, "beta", "jobs.atl", betaJobsV2)

	atPlan := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV1}, depLower(t, alphaF, betaV1))
	atApply := depToken(t, "alpha",
		[]*dsl.File{alphaF}, []*dsl.File{betaV2}, depLower(t, alphaF, betaV2))

	if atPlan != atApply {
		t.Errorf("beta.Sweep — which no workflow of alpha's runs — moved alpha's "+
			"token (%s -> %s)", atPlan[:12], atApply[:12])
	}
}

// An empty checkpoint yields an empty token, and an empty token is what both
// call sites read as "no comparison to make". Pinned because the alternative —
// hashing an empty IR into a real-looking hash — would make the first apply
// into a fresh database compare against a value no plan ever produced.
func TestNoCheckpointYieldsNoToken(t *testing.T) {
	alphaF := depFile(t, "alpha", "order.atl", alphaOrderV1)
	newIR := depLower(t, alphaF)
	codegen.AssignProtoNumbers(nil, newIR)
	d := codegen.ComputeDiff(nil, newIR)
	h, err := callerDependencyHash("alpha", nil,
		buildEntityOwnership("alpha", []*dsl.File{alphaF}, nil), []*dsl.File{alphaF}, d)
	if err != nil {
		t.Fatalf("callerDependencyHash: %v", err)
	}
	if h != "" {
		t.Errorf("token against no checkpoint = %q, want empty", h)
	}
}
