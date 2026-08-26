package provision

import (
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// When a console credential is due for replacement.
//
// Two reasons. A certificate issued under a longer policy is never inside the
// renewal window, so an expiry-only rule makes shortening ClientLifetime apply
// to no existing organisation while the constant, the config and the metric all
// say it has.

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

func TestAnExpiredCredentialIsDue(t *testing.T) {
	now := time.Now()
	if due, _ := consoleRotationDue(now.Add(-time.Hour), renewWithin, now); !due {
		t.Error("an expired certificate is not due for replacement")
	}
}

// A certificate from before the lifetime was shortened is never within the
// renewal window, so without the policy branch the shortened lifetime applies
// only to organisations created afterwards.
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

// The replacement has exactly the current lifetime, so a comparison without
// tolerance finds it over-long by whatever elapsed between minting and checking,
// and rotates it again on every pass.
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

// The tolerance absorbs the gap between minting and checking, which is
// sub-second. One approaching the lifetime would swallow the policy branch.
func TestThePolicyToleranceIsSmallRelativeToTheLifetime(t *testing.T) {
	if policyDriftTolerance >= certs.ClientLifetime/10 {
		t.Errorf("the drift tolerance is %v against a lifetime of %v; it is wide "+
			"enough to hide a genuine policy change", policyDriftTolerance, certs.ClientLifetime)
	}
}
