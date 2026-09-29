package main

import (
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// TestPlanExitCodesAreTheDocumentedContract pins the mapping CI branches on.
//
// docs/reference/cli-tide.md documents these codes and shows a CI script
// branching on them, so a change here changes which pull requests merge.
//
// It calls the function cmdPlan calls. A copy of the mapping here would pass
// while cmdPlan disagreed with it.
func TestPlanExitCodesAreTheDocumentedContract(t *testing.T) {
	for _, tc := range []struct {
		class adminpb.PlanClass
		want  int
		why   string
	}{
		{adminpb.PlanClass_PLAN_CLASS_ADDITIVE, 0, "additive plans merge"},
		{adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED, 1, "backfill needs --backfill, not a merge block"},
		{adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING, 2, "breaking another caller blocks the merge"},
		{adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE, 4, "destroying rows blocks the merge, and CI must be able to tell it from a compatibility break"},
		{adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE, 3, "an unparseable plan is an operational failure, not an outcome"},
		{adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED, 3, "a server that sent no class told us nothing to act on"},
		{adminpb.PlanClass(99), 3, "a class from a newer server must not be guessed at"},
	} {
		if got := exitCodeForClass(tc.class); got != tc.want {
			t.Errorf("%v exits %d, want %d — %s", tc.class, got, tc.want, tc.why)
		}
	}
}

// TestEveryDeclaredClassIsHandled fails when a class is added to the proto
// without a decision about what CI does with it.
//
// exitCodeForClass answers for an unknown class — exit 3 — so nothing breaks
// loudly. That is the right runtime behaviour and the wrong review experience:
// whoever adds a class should have to state what CI does with it rather than
// inherit "operational failure" by omission. PLAN_CLASS_DESTRUCTIVE inherited
// it, and `tide plan` reported a plan that parsed perfectly well as an
// operational failure for as long as that lasted.
//
// This reads classExitCode, the map the binary actually uses, rather than a
// list of classes maintained beside it. A parallel list is a second thing to
// forget.
func TestEveryDeclaredClassIsHandled(t *testing.T) {
	// Classes with no classExitCode entry, which fall to 3. Neither carries a
	// plan outcome to act on: one is a schema that did not compile, the other a
	// server that sent no class.
	exit3ByDesign := map[adminpb.PlanClass]bool{
		adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED: true,
		adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE: true,
	}
	values := adminpb.PlanClass(0).Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		c := adminpb.PlanClass(values.Get(i).Number())
		_, hasCode := classExitCode[c]
		switch {
		case !hasCode && !exit3ByDesign[c]:
			t.Errorf("%v is declared in admin.proto but has no entry in classExitCode "+
				"and is not listed as exit-3-by-design, so `tide plan` reports a plan "+
				"that parsed fine as an operational failure. Give it an exit code, add "+
				"an arm to cmdApply's dispatch, and update the table in "+
				"docs/reference/cli-tide.md.", c)
		case hasCode && exit3ByDesign[c]:
			t.Errorf("%v is in classExitCode AND listed as exit-3-by-design. One of the "+
				"two is stale, and which one is right decides whether CI blocks", c)
		}
	}
}

// Every class the server can send must be painted, including the new one.
//
// A class missing from classPaint printed in the terminal's default colour,
// which reads as "nothing notable here" — the exact opposite of what a
// destructive plan is, and the failure mode is silent because the word is still
// correct.
func TestEveryDeclaredClassHasAColour(t *testing.T) {
	// UNSPECIFIED is the one class no server legitimately sends; it means the
	// field was never set. It is left unpainted so it reads as the absence it
	// is rather than as a severity.
	for i, values := 0, adminpb.PlanClass(0).Descriptor().Values(); i < values.Len(); i++ {
		c := adminpb.PlanClass(values.Get(i).Number())
		if c == adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED {
			continue
		}
		if _, ok := classPaint[c]; !ok {
			t.Errorf("%v is declared in admin.proto but has no entry in classPaint, so "+
				"it prints in the terminal's default colour. A reader scanning for risk "+
				"sees nothing notable", c)
		}
	}
}

// planClassName is what the terminal shows. The wire name is right on the wire
// and wrong in a table.
func TestPlanClassNameStripsThePrefix(t *testing.T) {
	for c, want := range map[adminpb.PlanClass]string{
		adminpb.PlanClass_PLAN_CLASS_ADDITIVE:              "additive",
		adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:     "backfill_required",
		adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING: "cross_caller_breaking",
		adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE:           "destructive",
		adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:           "unparseable",
	} {
		if got := planClassName(c); got != want {
			t.Errorf("planClassName(%v) = %q, want %q", c, got, want)
		}
	}
}

// Every change class the differ can produce must be countable in the tally.
//
// The other tests in this file walk adminpb.PlanClass, the class of the PLAN.
// This walks codegen.ChangeClass, the class of an individual CHANGE — a second
// enum, on a second axis, feeding a second renderer. Covering only the first is
// how `tide diff` came to print a destructive-only diff as "(no changes)" while
// `tide plan` had known about the class for a release.
//
// changeClassDisplay drives both the `tide diff` tally and the rollback
// preview, so a class missing here is invisible in both.
func TestEveryChangeClassIsDisplayable(t *testing.T) {
	shown := map[codegen.ChangeClass]bool{}
	glyphs := map[string]codegen.ChangeClass{}
	for _, s := range changeClassDisplay {
		if prev, dup := glyphs[s.glyph]; dup {
			t.Errorf("%v and %v both render with glyph %q; the tally then cannot be "+
				"read without the words, which is what the glyph is for", prev, s.class, s.glyph)
		}
		glyphs[s.glyph] = s.class
		shown[s.class] = true
	}

	// ChangeClass is an iota enum with no sentinel to walk, so the bound comes
	// from String() returning "unknown" past its last member — the same way
	// internal/server/admin/planclass_mapping_test.go finds it.
	for c := codegen.ClassAdditive; ; c++ {
		if c.String() == "unknown" {
			break
		}
		if !shown[c] {
			t.Errorf("codegen.%v has no entry in changeClassDisplay, so changes of that "+
				"class are counted by neither the `tide diff` tally nor the rollback "+
				"preview. An operator reading either is told the change is not there", c)
		}
		if c > 32 {
			t.Fatal("codegen.ChangeClass has no 'unknown' sentinel; this loop cannot " +
				"find its upper bound and would spin")
		}
	}
}

// The approval hint names the command that reaches the server's gate for each
// class: a backfill-required plan is filed only by `tide apply --backfill`.
func TestApprovalNextStepNamesTheCommandThatFilesTheRequest(t *testing.T) {
	for _, tc := range []struct {
		class adminpb.PlanClass
		want  string
	}{
		{adminpb.PlanClass_PLAN_CLASS_ADDITIVE, "`tide apply` files the request; admin decides under Approvals"},
		{adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED, "`tide apply --backfill` files the request; admin decides under Approvals"},
		{adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING, "`tide apply` files the request; admin decides under Approvals"},
		{adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE, "`tide apply` files the request; admin decides under Approvals"},
	} {
		if got := approvalNextStep(tc.class, "admin"); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.class, got, tc.want)
		}
	}
}
