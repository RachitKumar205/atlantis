package main

import (
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestPlanExitCodesAreTheDocumentedContract pins the mapping CI branches on.
//
// docs/reference/cli-tide.md documents these codes and
// docs/guides/set-up-caller-ci.md tells readers to gate merges on them, so a
// change here changes whether other people's pull requests can merge. The
// mapping just went through a mechanical string-to-enum rewrite, which is
// exactly the kind of edit that silently renumbers one case.
//
// It calls the function cmdPlan calls. A copy of the switch living here would
// pass while cmdPlan disagreed with it, which is the failure this is for.
func TestPlanExitCodesAreTheDocumentedContract(t *testing.T) {
	for _, tc := range []struct {
		class adminpb.PlanClass
		want  int
		why   string
	}{
		{adminpb.PlanClass_PLAN_CLASS_ADDITIVE, 0, "additive plans merge"},
		{adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED, 1, "backfill needs --backfill, not a merge block"},
		{adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING, 2, "breaking another caller blocks the merge"},
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
// without a decision here.
//
// The default arm means a new class already has an answer — exit 3 — so nothing
// would break loudly. That is the right runtime behaviour and the wrong review
// experience: whoever adds PLAN_CLASS_DESTRUCTIVE should have to state what CI
// does with it rather than inherit "operational failure" by omission.
func TestEveryDeclaredClassIsHandled(t *testing.T) {
	decided := map[adminpb.PlanClass]bool{
		adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED:           true,
		adminpb.PlanClass_PLAN_CLASS_ADDITIVE:              true,
		adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:     true,
		adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING: true,
		adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:           true,
	}
	values := adminpb.PlanClass(0).Descriptor().Values()
	for i := 0; i < values.Len(); i++ {
		c := adminpb.PlanClass(values.Get(i).Number())
		if !decided[c] {
			t.Errorf("%v is declared in admin.proto but no exit code was chosen for it. "+
				"Add it to exitCodeForClass, to cmdPlan's switch, to cmdApply's dispatch, "+
				"and to the exit-code table in docs/reference/cli-tide.md.", c)
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
		adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:           "unparseable",
	} {
		if got := planClassName(c); got != want {
			t.Errorf("planClassName(%v) = %q, want %q", c, got, want)
		}
	}
}
