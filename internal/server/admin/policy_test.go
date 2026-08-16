package admin

import (
	"os"
	"reflect"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/codegen"
)

const policyMigration = "../../../migrations/infra/0026_change_policy.up.sql"

// Every bucket a diff can be sorted into must have a rule.
//
// Reflective over codegen.Diff rather than a list, for the reason
// TestDiffAllCoversEveryBucket exists: adding Destructive found six places that
// had enumerated the three buckets by hand, and every one of them silently
// dropped the one kind that destroys data. A fifth bucket arriving with no
// policy key would not drop anything — it would land on the fail-closed
// default and require approval forever, with no way for an operator to turn it
// off. That is the safe failure, and it is still a failure worth being told
// about at build time rather than by a support ticket.
func TestEveryChangeClassHasAPolicyKey(t *testing.T) {
	typ := reflect.TypeOf(codegen.Diff{})
	var buckets int
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type == reflect.TypeOf([]codegen.Change{}) {
			buckets++
		}
	}
	if buckets < 4 {
		t.Fatalf("found %d change buckets on codegen.Diff; this test is not reading "+
			"what it thinks it is", buckets)
	}
	if got := len(ChangeClasses()); got != buckets {
		t.Errorf("codegen.Diff has %d change buckets but ChangeClasses() names %d. "+
			"A bucket with no policy key can never be exempted from approval, "+
			"whatever an operator sets.", buckets, got)
	}

	// And each must reach a distinct, settable PlanClass — two classes
	// collapsing onto one wire value would make a rule for one silently govern
	// the other.
	seen := map[adminpb.PlanClass]codegen.ChangeClass{}
	for _, cc := range ChangeClasses() {
		pc := planClassToPB(translateClass(cc))
		if pc == adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED || pc == adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE {
			t.Errorf("%s maps to %s, which no rule can apply to", cc, pc)
		}
		if prev, dup := seen[pc]; dup {
			t.Errorf("%s and %s both map to %s; a rule for one would govern both", prev, cc, pc)
		}
		seen[pc] = cc
		if !policyClassIsSettable(pc) {
			t.Errorf("%s maps to %s, which SetChangePolicy refuses to write", cc, pc)
		}
	}
}

// Fail-closed, stated three ways, because all three are the same rule and a
// future edit is likely to handle one and forget the others.
// Every class the gate can actually be asked about must be settable.
//
// This is what keeps gateOnChangePolicy's `!policyClassIsSettable` arm
// unreachable — and unreachable is what it is today, which is precisely why it
// has no test of its own. policyClassIsSettable is DEFINED as the image of
// ChangeClasses(), and the gate only ever asks about members of
// ClassesPresent(), which returns the same four values. Writing a test that
// drives that arm would mean fabricating a state the code cannot produce, which
// is the vacuous-guard shape this whole exercise is about removing.
//
// So the invariant is pinned instead of the branch. The hazard it catches is
// real and would be silent: a fifth bucket added to ClassesPresent that
// ChangeClasses does not know about makes the arm reachable, and because it
// fires before any rule is consulted, EVERY apply carrying that class is
// refused with a message about plan classes rather than about the change.
func TestEveryClassPresentIsSettable(t *testing.T) {
	// All four buckets populated, so ClassesPresent returns its full set rather
	// than whichever one a sparser fixture happened to include.
	d := &codegen.Diff{
		Additive:         []codegen.Change{{}},
		BackfillRequired: []codegen.Change{{}},
		Destructive:      []codegen.Change{{}},
		Breaking:         []codegen.Change{{}},
	}
	present := d.ClassesPresent()
	if len(present) != len(ChangeClasses()) {
		t.Fatalf("ClassesPresent returned %d classes for a diff with every bucket "+
			"populated, want %d — the two enumerations have diverged",
			len(present), len(ChangeClasses()))
	}
	for _, c := range present {
		pb := planClassToPB(translateClass(c))
		if !policyClassIsSettable(pb) {
			t.Errorf("ClassesPresent yields %v, which maps to %s, which no rule can "+
				"govern. gateOnChangePolicy refuses this before it reads any policy, "+
				"so every apply carrying this class fails with a message about plan "+
				"classes and no operator can turn it off", c, pb)
		}
	}
}

func TestAnythingOtherThanAStoredRuleRequiresApproval(t *testing.T) {
	stored := map[adminpb.PlanClass]ChangePolicy{
		adminpb.PlanClass_PLAN_CLASS_ADDITIVE: {
			Class: adminpb.PlanClass_PLAN_CLASS_ADDITIVE, RequireApproval: false,
		},
	}
	cases := []struct {
		name  string
		class adminpb.PlanClass
	}{
		{"a class whose row was deleted", adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE},
		{"a class the table never had", adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING},
		{"a class no rule can apply to", adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE},
		{"the zero value", adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED},
	}
	for _, c := range cases {
		got := effectiveChangePolicy(stored, c.class)
		if !got.RequireApproval {
			t.Errorf("%s (%s) resolved to require_approval=false. Every way of failing "+
				"to find a rule has to fail the same way, and that way is closed.",
				c.name, c.class)
		}
		if got.ApproverRole != DefaultApproverRole {
			t.Errorf("%s resolved to approver_role %q, want %q — a rule nobody can "+
				"satisfy is a deployment that cannot apply at all",
				c.name, got.ApproverRole, DefaultApproverRole)
		}
	}

	// The one stored rule must still be honoured, or "fail closed" has become
	// "ignore the table".
	if effectiveChangePolicy(stored, adminpb.PlanClass_PLAN_CLASS_ADDITIVE).RequireApproval {
		t.Error("a stored rule saying require_approval=false was overridden; the " +
			"fail-closed default is for absence, not for every read")
	}
}

// A stored rule with no approver_role names a role that exists.
//
// The column defaults to 'admin', so this only fires if someone writes an
// empty string directly. Worth pinning anyway: an empty required role is not
// "anyone may approve", it is "no console role matches", and the difference
// between those two readings is whether a deployment can apply at all.
func TestAnEmptyApproverRoleFallsBackRatherThanMatchingNobody(t *testing.T) {
	stored := map[adminpb.PlanClass]ChangePolicy{
		adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE: {
			Class: adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE, RequireApproval: true, ApproverRole: "",
		},
	}
	if got := effectiveChangePolicy(stored, adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE); got.ApproverRole != DefaultApproverRole {
		t.Errorf("approver_role = %q, want %q", got.ApproverRole, DefaultApproverRole)
	}
}

// The console must be able to reach the RPC it is the only surface for.
//
// 0019 seeds the console's grants and a test in internal/server/authz holds it
// to the RPCs the console calls. SCHEMA_APPROVE cannot go there — 0019 has
// already run everywhere — so it is granted in 0026, and this is what says so
// out loud rather than leaving the split between two migrations to be
// rediscovered.
func TestTheMigrationGrantsTheConsoleTheApproveCapability(t *testing.T) {
	body, err := os.ReadFile(policyMigration)
	if err != nil {
		t.Fatalf("read %s: %v", policyMigration, err)
	}
	sql := stripSQLComments(string(body))
	if !strings.Contains(sql, "CAPABILITY_SCHEMA_APPROVE") {
		t.Error("0026 does not grant CAPABILITY_SCHEMA_APPROVE; the console's policy " +
			"editor returns PermissionDenied on every deployment")
	}
	if !strings.Contains(sql, "atlantis-console") {
		t.Error("0026 grants CAPABILITY_SCHEMA_APPROVE to nobody")
	}
	// And it must not have been handed to anything that can apply. The whole
	// separation rests on no identity holding both.
	if strings.Contains(sql, "CAPABILITY_SCHEMA_APPLY") {
		t.Error("0026 mentions CAPABILITY_SCHEMA_APPLY; an identity holding both that " +
			"and SCHEMA_APPROVE is a caller that approves its own changes")
	}
}

// stripSQLComments removes `-- ...` so prose about a capability is not read as
// a grant of it. The comments in these migrations name every capability they
// discuss, at length.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
