package admin

import (
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The whole tier matrix, one row per cell that carries a decision. The gate
// composes this with the change policy by most-restrictive-wins, so a wrong
// false here is an unattended apply the operator did not choose.
func TestTierMatrix(t *testing.T) {
	const (
		add = adminpb.PlanClass_PLAN_CLASS_ADDITIVE
		bf  = adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED
		brk = adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING
		del = adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE
	)
	cases := []struct {
		tier    applyPolicy
		class   adminpb.PlanClass
		verdict string
		want    bool
	}{
		{PolicyAlwaysAsk, add, "", true},
		{PolicyAlwaysAsk, add, "pass", true},
		{PolicyAlwaysAsk, del, "pass", true},

		{PolicyAutoSafe, add, "", false},
		{PolicyAutoSafe, bf, "", true},
		{PolicyAutoSafe, bf, "pass", true},
		{PolicyAutoSafe, brk, "pass", true},
		{PolicyAutoSafe, del, "pass", true},

		{PolicyAutoVerified, add, "", false},
		{PolicyAutoVerified, bf, "", true},
		{PolicyAutoVerified, bf, "pass", false},
		{PolicyAutoVerified, bf, "pass_with_warnings", false},
		{PolicyAutoVerified, bf, "fail_data", true},
		{PolicyAutoVerified, bf, "unverified", true},
		{PolicyAutoVerified, brk, "pass", true},
		{PolicyAutoVerified, del, "pass", true},

		{PolicyAutoAll, add, "", false},
		{PolicyAutoAll, bf, "pass", false},
		{PolicyAutoAll, del, "", true},
		{PolicyAutoAll, del, "pass", false},
		{PolicyAutoAll, del, "pass_with_warnings", false},
		{PolicyAutoAll, del, "fail_structural", true},
		// A per-caller policy cannot consent on behalf of the callers a
		// breaking change breaks, whatever the verdict.
		{PolicyAutoAll, brk, "pass", true},
		{PolicyAutoAll, brk, "", true},

		// sandbox_only is refused before the matrix is consulted; if it ever
		// reaches here, everything queues. Same for a value no build knows.
		{PolicySandboxOnly, add, "pass", true},
		{applyPolicy("yolo"), add, "pass", true},
	}
	for _, tc := range cases {
		if got := tierRequiresApproval(tc.tier, tc.class, tc.verdict); got != tc.want {
			t.Errorf("tierRequiresApproval(%s, %s, %q) = %v, want %v",
				tc.tier, tc.class, tc.verdict, got, tc.want)
		}
	}
}

func TestEffectiveApplyPolicyResolvesAndClamps(t *testing.T) {
	cases := []struct {
		stored string
		floor  string
		want   applyPolicy
	}{
		{"", "", PolicyAutoSafe},
		{"auto_all", "", PolicyAutoAll},
		{"sandbox_only", "", PolicySandboxOnly},
		// Unknown stored values queue everything while they are sorted out.
		{"yolo", "", PolicyAlwaysAsk},
		// The floor only lowers.
		{"auto_all", "auto_safe", PolicyAutoSafe},
		{"", "always_ask", PolicyAlwaysAsk},
		{"always_ask", "auto_all", PolicyAlwaysAsk},
		{"sandbox_only", "auto_safe", PolicySandboxOnly},
	}
	for _, tc := range cases {
		s := &Service{applyPolicyFloor: tc.floor}
		if got := s.effectiveApplyPolicy(tc.stored); got != tc.want {
			t.Errorf("effectiveApplyPolicy(%q) with floor %q = %s, want %s",
				tc.stored, tc.floor, got, tc.want)
		}
	}
}

func TestParseApplyPolicyFloorRefusesATypo(t *testing.T) {
	t.Setenv("ATLANTIS_APPLY_POLICY_FLOOR", "")
	if v, err := ParseApplyPolicyFloor(); err != nil || v != "" {
		t.Errorf("unset floor = (%q, %v), want empty and nil", v, err)
	}
	t.Setenv("ATLANTIS_APPLY_POLICY_FLOOR", "auto_safe")
	if v, err := ParseApplyPolicyFloor(); err != nil || v != "auto_safe" {
		t.Errorf("floor auto_safe = (%q, %v)", v, err)
	}
	t.Setenv("ATLANTIS_APPLY_POLICY_FLOOR", "auto-safe")
	if _, err := ParseApplyPolicyFloor(); err == nil {
		t.Error("a floor that names no tier parsed; the server would boot and never clamp")
	} else if !strings.Contains(err.Error(), "auto-safe") {
		t.Errorf("the refusal does not name the value: %v", err)
	}
}
