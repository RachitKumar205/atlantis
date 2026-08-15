package admin

import (
	"context"
	"errors"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// jobReadScope decides whether a job query filters by owner. Returning "" means
// "read every caller's rows", so every path that can return "" is a path that
// can widen a read to the whole table.
//
// These cover the arms a database cannot reach: a missing dependency, a
// capability lookup that fails, an unresolved identity. Each of them is a
// wrong answer away from the exposure this whole change exists to close.

func scopeSvc(caller string, hasCap func(context.Context, adminpb.Capability) (bool, error)) *Service {
	s := &Service{hasCapability: hasCap}
	if caller != "\x00" {
		s.callerFromContext = func(context.Context) string { return caller }
	}
	return s
}

func TestJobReadScopeFailsClosed(t *testing.T) {
	operator := func(context.Context, adminpb.Capability) (bool, error) { return true, nil }
	notOperator := func(context.Context, adminpb.Capability) (bool, error) { return false, nil }
	broken := func(context.Context, adminpb.Capability) (bool, error) {
		return false, errors.New("caller_capabilities is unreachable")
	}

	for _, tc := range []struct {
		name string
		svc  *Service
		want string
		why  string
	}{
		{
			name: "an ordinary caller is scoped to itself",
			svc:  scopeSvc("shop", notOperator),
			want: "shop",
		},
		{
			name: "an operator reads across callers",
			svc:  scopeSvc("atlantis-console", operator),
			want: "",
			why:  "the console's queue and DLQ views would otherwise render empty",
		},
		{
			name: "a failed capability lookup scopes rather than widens",
			svc:  scopeSvc("shop", broken),
			want: "shop",
			why: "the lookup reads a table; 'the database did not answer' must not be " +
				"what turns a scoped read into a global one",
		},
		{
			name: "no capability source means nobody is an operator",
			svc:  scopeSvc("shop", nil),
			want: "shop",
			why: "forgetting to wire HasCapability must narrow what is visible, not " +
				"widen it — the symptom should be an empty console, not a leak",
		},
		{
			name: "an anonymous caller cannot be scoped",
			svc:  scopeSvc("anonymous", notOperator),
			want: "",
			why: "insecure dev mode, where the caller name is a header the client " +
				"writes itself; filtering on it would deny the honest and admit the rest",
		},
		{
			name: "no identity source at all cannot be scoped",
			svc:  scopeSvc("\x00", notOperator),
			want: "",
			why:  "same as anonymous — there is nothing to filter on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.svc.jobReadScope(context.Background())
			if got != tc.want {
				t.Errorf("jobReadScope = %q, want %q.\n  %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestJobReadScopeAsksForOperatorAndNothingElse pins WHICH capability widens.
//
// hasCapability answers about whatever it is handed, so a call passing the
// wrong constant would still compile, still return a bool, and still look
// right — while widening every caller holding some unrelated grant. JOBS_READ
// is the dangerous confusion: it is in the bundle every registered caller
// receives, so mistaking it for the operator check would unscope everybody.
func TestJobReadScopeAsksForOperatorAndNothingElse(t *testing.T) {
	var asked []adminpb.Capability
	svc := scopeSvc("shop", func(_ context.Context, c adminpb.Capability) (bool, error) {
		asked = append(asked, c)
		return c == adminpb.Capability_CAPABILITY_JOBS_READ, nil
	})

	if got := svc.jobReadScope(context.Background()); got != "shop" {
		t.Errorf("jobReadScope = %q, want %q — holding JOBS_READ unscoped this caller, "+
			"and every registered caller holds it", got, "shop")
	}
	for _, c := range asked {
		if c != adminpb.Capability_CAPABILITY_OPERATOR {
			t.Errorf("asked about %s; the only capability that widens a job read is "+
				"CAPABILITY_OPERATOR", c)
		}
	}
	if len(asked) == 0 {
		t.Error("no capability was checked at all, so nothing decides who is an operator")
	}
}
