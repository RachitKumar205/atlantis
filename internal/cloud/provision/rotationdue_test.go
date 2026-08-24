package provision

import (
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// When a console credential is due for replacement.
//
// Two reasons, and the second is the one that is easy to leave out and
// impossible to notice missing: a certificate issued under a longer policy is
// never inside the renewal window, so an expiry-only test would have made
// shortening ClientLifetime a change that applied to no existing organisation
// while every other signal — the constant, the config, the metric — said it had.

const renewWithin = 10 * 24 * time.Hour

func TestACredentialWellInsideItsLifeIsNotDue(t *testing.T) {
	now := time.Now()
	// Freshly issued: exactly the policy lifetime left.
	if due, why := consoleRotationDue(now.Add(certs.ClientLifetime), renewWithin, now); due {
		t.Errorf("a certificate issued this instant is already due (%q); the fleet "+
			"would rotate on every reconcile pass", why)
	}
	// Halfway through.
	half := now.Add(certs.ClientLifetime / 2)
	if due, why := consoleRotationDue(half, renewWithin, now); due {
		t.Errorf("a certificate halfway through its life is due (%q)", why)
	}
}

func TestACredentialInsideTheRenewalWindowIsDue(t *testing.T) {
	now := time.Now()
	due, why := consoleRotationDue(now.Add(renewWithin-time.Hour), renewWithin, now)
	if !due {
		t.Fatal("a certificate inside the renewal window is not due")
	}
	if !strings.Contains(why, "renewal window") {
		t.Errorf("the reason is %q, which does not say the window was reached", why)
	}
}

// An already-expired credential is still due, rather than wrapping into some
// other branch.
func TestAnExpiredCredentialIsDue(t *testing.T) {
	now := time.Now()
	if due, _ := consoleRotationDue(now.Add(-time.Hour), renewWithin, now); !due {
		t.Error("an expired certificate is not due for replacement")
	}
}

// The migration case: a certificate from before the lifetime was shortened.
//
// Every organisation provisioned before that change holds one. Without this
// branch none of them is ever within the renewal window, so the shortened
// lifetime would apply only to organisations created afterwards — and nothing
// would report the difference.
func TestACredentialFromTheOldTenYearPolicyIsDue(t *testing.T) {
	now := time.Now()
	tenYears := now.Add(3650 * 24 * time.Hour)

	due, why := consoleRotationDue(tenYears, renewWithin, now)
	if !due {
		t.Fatal("a ten-year certificate is not due, so no organisation provisioned " +
			"before the lifetime was shortened would ever be migrated")
	}
	if !strings.Contains(why, "lifetime policy") {
		t.Errorf("the reason is %q, which does not explain why an organisation "+
			"nowhere near expiry rotated", why)
	}
}

// Rotating does not re-arm the policy branch.
//
// The replacement has exactly the current lifetime, so a comparison without
// tolerance would find it over-long by whatever time elapsed between minting and
// checking and rotate it again — every pass, for ever, with nothing to show for
// it but a climbing counter.
func TestAFreshlyRotatedCredentialIsNotImmediatelyDueAgain(t *testing.T) {
	now := time.Now()
	// What ReissueConsoleLeaves produces, including the backdating it applies.
	minted := now.Add(certs.ClientLifetime)

	for _, elapsed := range []time.Duration{0, time.Second, time.Minute} {
		at := now.Add(elapsed)
		if due, why := consoleRotationDue(minted, renewWithin, at); due {
			t.Errorf("%v after being issued, a certificate is due again (%q)", elapsed, why)
		}
	}
}

// The tolerance is not so wide that a real policy change slips through it.
//
// It exists to absorb the gap between minting and checking, which is
// sub-second. A tolerance approaching the lifetime would swallow the migration
// branch entirely and reintroduce the bug it was added to fix.
func TestThePolicyToleranceIsSmallRelativeToTheLifetime(t *testing.T) {
	if policyDriftTolerance >= certs.ClientLifetime/10 {
		t.Errorf("the drift tolerance is %v against a lifetime of %v; it is wide "+
			"enough to hide a genuine policy change", policyDriftTolerance, certs.ClientLifetime)
	}
}
