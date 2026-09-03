package admin

import "testing"

func TestProtectedPatternValidation(t *testing.T) {
	valid := []string{"payments.Invoice", "payments.*", "shop.Order_v2", "a.b"}
	for _, p := range valid {
		if _, err := validateProtectedPattern(p); err != nil {
			t.Errorf("validateProtectedPattern(%q): %v", p, err)
		}
	}
	invalid := []string{"", "payments", "payments.", ".Invoice", "*", "*.Invoice",
		"payments.In*ice", "payments.*x", "pay ments.Invoice", "payments.Invoice; DROP"}
	for _, p := range invalid {
		if got, err := validateProtectedPattern(p); err == nil {
			t.Errorf("validateProtectedPattern(%q) accepted as %q", p, got)
		}
	}
}

func TestMatchProtected(t *testing.T) {
	rules := []protectedRule{
		{Pattern: "payments.Invoice", Floor: "admin_only"},
		{Pattern: "audit.*", Floor: "require_approval"},
	}
	cases := []struct {
		ids  []string
		want string // matched pattern; "" for no match
	}{
		{[]string{"payments.Invoice"}, "payments.Invoice"},
		{[]string{"payments.Refund"}, ""},
		{[]string{"audit.Log"}, "audit.*"},
		{[]string{"auditor.Log"}, ""},
		{[]string{"shop.Order", "audit.Log"}, "audit.*"},
		{nil, ""},
	}
	for _, tc := range cases {
		got := matchProtected(rules, tc.ids)
		switch {
		case got == nil && tc.want != "":
			t.Errorf("matchProtected(%v) = nil, want %q", tc.ids, tc.want)
		case got != nil && got.Pattern != tc.want:
			t.Errorf("matchProtected(%v) = %q, want %q", tc.ids, got.Pattern, tc.want)
		}
	}
}
