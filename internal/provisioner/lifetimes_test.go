package provisioner

import (
	"math"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// The two numbers that decide whether console credentials renew safely.
//
// certs.ClientLifetime is how long one lasts; DefaultConsoleCertRenewWithin is
// how much life must be left before the provisioner replaces it. Neither is
// meaningful alone, they live in different packages, and nothing else makes
// them agree — so a change to one of them is exactly the edit that passes review
// and produces an outage weeks later.
//
// Both failure directions are silent, which is why they are pinned here rather
// than left to the comments that explain them.

// A freshly issued certificate must not already be due for renewal.
//
// If the window is as wide as the lifetime, every certificate is always due:
// the provisioner reissues every organisation on every reconcile pass, writing
// Secrets and re-registering the fleet every five minutes. Nothing breaks, no
// error is logged, and the only outward sign is a rotation counter climbing at
// the reconcile rate — which is why the metric's own comment calls that out.
func TestAFreshConsoleCertificateIsNotImmediatelyDueForRenewal(t *testing.T) {
	if DefaultConsoleCertRenewWithin >= certs.ClientLifetime {
		t.Fatalf("the renewal window (%v) is not shorter than the certificate "+
			"lifetime (%v), so every certificate is due the moment it is issued "+
			"and the fleet rotates on every reconcile pass",
			DefaultConsoleCertRenewWithin, certs.ClientLifetime)
	}
}

// The window has to leave room for passes to fail.
//
// Its whole purpose is margin: how long the provisioner can be wedged, mid-
// deploy, or locked out of the cluster before an organisation loses console
// access. A window measured in a few reconcile intervals would mean a single
// bad afternoon expires the fleet.
//
// Asserted as a multiple of the reconcile interval rather than as a fixed
// duration, so shortening the interval cannot silently narrow the margin.
func TestTheRenewalWindowSurvivesManyMissedPasses(t *testing.T) {
	const wantPasses = 100
	passes := DefaultConsoleCertRenewWithin / DefaultReconcileInterval
	if passes < wantPasses {
		t.Errorf("the renewal window (%v) covers only %d reconcile passes of %v; "+
			"a credential should survive far more missed passes than that before "+
			"it expires", DefaultConsoleCertRenewWithin, passes, DefaultReconcileInterval)
	}
}

// The certificate must outlast the window by a real margin.
//
// The gap between them is the working life of a credential — the time it spends
// in service before anything tries to replace it. A lifetime only slightly
// longer than the window would rotate constantly while technically satisfying
// the test above.
func TestAConsoleCertificateSpendsMostOfItsLifeInService(t *testing.T) {
	inService := certs.ClientLifetime - DefaultConsoleCertRenewWithin
	if inService <= DefaultConsoleCertRenewWithin {
		t.Errorf("a console certificate is in service for %v and then renewed over "+
			"a window of %v; the window should be the tail of its life, not most "+
			"of it", inService, DefaultConsoleCertRenewWithin)
	}
}

// The lifetime stays inside the guidance the rest of the system reasoned from.
//
// Migration 0032 removed certificate pinning on the argument that a short life
// replaces active revocation, citing smallstep's "one month or less" for
// service certificates. This is the certificate that argument now applies to,
// so a lifetime creeping past a month would quietly undo the trade that
// migration made.
func TestTheConsoleCertificateLifetimeStaysWithinAMonth(t *testing.T) {
	const month = 31 * 24 * time.Hour
	if certs.ClientLifetime > month {
		t.Errorf("console certificates last %v, which is longer than the month "+
			"migration 0032 relied on when it dropped pinning", certs.ClientLifetime)
	}
}

// The fleet countdown must not read as zero before it has been measured.
//
// A Prometheus gauge defaults to zero, and zero seconds left means "expiring
// now". Every `console_cert_seconds_left < threshold` rule would therefore fire
// the moment this process starts and keep firing until the first reconcile
// pass — up to a reconcile interval of paging on every deploy, restart and
// rollout. An alert that goes off routinely is one somebody silences, and this
// is the alert that stands between a rotation quietly failing and a fleet-wide
// outage.
//
// NaN is the value that means "not measured": comparisons against it are false,
// so nothing fires, and it renders as a gap rather than a cliff.
func TestTheFleetCountdownStartsUnmeasuredRatherThanZero(t *testing.T) {
	// Driven through the function init calls, not read off the live gauge:
	// every other test in this package writes it, so reading it here would make
	// this test depend on which file the toolchain runs first.
	markConsoleCertUnmeasured()

	var m dto.Metric
	if err := consoleCertSecondsLeft.Write(&m); err != nil {
		t.Fatalf("read the gauge: %v", err)
	}
	got := m.GetGauge().GetValue()

	if got == 0 {
		t.Fatal("the fleet countdown reads exactly zero before anything has " +
			"measured it, which every threshold rule reads as an imminent expiry; " +
			"it should be NaN until the first reconcile pass")
	}
	if !math.IsNaN(got) {
		t.Errorf("the fleet countdown starts at %v; it should be NaN so that no "+
			"threshold rule fires before a pass has measured anything", got)
	}
	// And the property that actually matters: a threshold comparison is false.
	if got < (5 * 24 * time.Hour).Seconds() {
		t.Error("an unmeasured countdown compares below a threshold, so an alert " +
			"would fire on a provisioner that has simply just started")
	}
}
