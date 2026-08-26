package admin

import (
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

// Every change class must reach a distinct, meaningful wire value.
//
// The path is codegen.ChangeClass -> translateClass -> ClassName ->
// planClassToPB -> adminpb.PlanClass: two hops, each a switch with a
// fall-through. A class missing an arm in both is neither a compile error nor a
// panic. translateClass's fall-through means "the DSL did not compile", so the
// plan arrives at the CLI as PLAN_CLASS_UNPARSEABLE and `tide apply` prints
//
//	tide: plan is unparseable — custom-query SQL validation failed.
//
// with both error lists empty, and exits 3, naming a subsystem that was not
// involved for a plan that parsed.
//
// An arm in the first hop alone moves it from UNPARSEABLE to UNSPECIFIED, still
// exit 3. Both are required together, so this asserts the composition rather
// than either hop.
//
// A loop rather than a table of cases: this walks every value the Go enum can
// hold, so a class added later fails here on the day it is added.
// TestEveryDeclaredClassIsHandled is the counterpart on the proto side, and the
// gap between the two enums is what a single-sided check misses.
func TestEveryCodegenClassReachesADistinctPlanClass(t *testing.T) {
	// Every value of codegen.ChangeClass. It is an iota enum with no String()
	// sentinel to walk, so the bound comes from its last member — and String()
	// returning "unknown" for anything past it is what proves the bound is
	// right.
	var all []codegen.ChangeClass
	for c := codegen.ClassAdditive; ; c++ {
		if c.String() == "unknown" {
			break
		}
		all = append(all, c)
		if len(all) > 32 {
			t.Fatal("codegen.ChangeClass has no 'unknown' sentinel; this loop " +
				"cannot find its upper bound and would spin")
		}
	}
	if len(all) < 4 {
		t.Fatalf("found only %d change classes; expected at least the four "+
			"documented in diff.go. Has String() changed?", len(all))
	}

	seen := map[adminpb.PlanClass]codegen.ChangeClass{}
	for _, c := range all {
		name := translateClass(c)
		got := planClassToPB(name)

		if got == adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED {
			t.Errorf("%s reaches PLAN_CLASS_UNSPECIFIED. planClassToPB has no arm "+
				"for ClassName %q, so the CLI sees an unknown class and exits 3 "+
				"telling the user to upgrade tide", c, name)
			continue
		}
		if got == adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE {
			t.Errorf("%s reaches PLAN_CLASS_UNPARSEABLE. That value means the DSL "+
				"did not compile, so the caller is told their schema is broken "+
				"when it parsed fine — and `tide apply` prints an empty list of "+
				"custom-SQL errors", c)
			continue
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s both map to %v. Two classes the diff engine "+
				"distinguishes are indistinguishable to every client", prev, c, got)
			continue
		}
		seen[got] = c
	}
}

// The class the server refuses must be the class it reports.
//
// A mismatch here is worse than either bug alone: the plan says one thing and
// the refusal says another, and an operator reading the two together cannot
// tell which is authoritative.
func TestRefusedClassesAreReportedAsThemselves(t *testing.T) {
	for _, c := range []codegen.ChangeClass{
		codegen.ClassCrossCallerBreaking,
		codegen.ClassDestructive,
	} {
		got := planClassToPB(translateClass(c))
		want := map[codegen.ChangeClass]adminpb.PlanClass{
			codegen.ClassCrossCallerBreaking: adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING,
			codegen.ClassDestructive:         adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE,
		}[c]
		if got != want {
			t.Errorf("%s is refused by ApplyMigration but reported as %v, want %v",
				c, got, want)
		}
	}
}
