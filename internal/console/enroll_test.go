package console

import (
	"strings"
	"testing"
)

// The enrolment settings, and the state they exist to make impossible.
//
// The predecessor of this feature — server-side key generation, reached through
// ATL_SIGNER_ADDR — was documented as optional and read at request time. No
// deployment ever set it, so the console shipped with a button that answered
// 503 for its entire life and nothing at startup ever said so. These tests are
// what stop that arrangement coming back in a new shape.

// enrolmentEnv is a complete enrolment configuration.
//
// Paths, not files: validateEnrollment decides on presence, and whether the
// files parse is New()'s problem. Keeping them apart is deliberate — a test
// about "is this configured" that also needed a real certificate authority
// would be testing two things and failing for either reason.
var enrolmentEnv = map[string]string{
	"ATL_SIGNER_ADDR":         "https://signer.internal:7070",
	"ATL_SIGNER_CERT":         "/certs/console.crt",
	"ATL_SIGNER_KEY":          "/certs/console.key",
	"ATL_SIGNER_CA":           "/certs/signer-ca.crt",
	"CONSOLE_ENROLL_LISTEN":   ":3443",
	"CONSOLE_ENROLL_TLS_CERT": "/certs/enroll.crt",
	"CONSOLE_ENROLL_TLS_KEY":  "/certs/enroll.key",

	// This was outside the required set once, and silently broken by it: a
	// console could enrol perfectly and print an instruction naming an address
	// nobody could reach. Its companion in that mistake,
	// CONSOLE_ENROLL_CLIENT_CA, no longer exists — see the test below.
	"CONSOLE_ENROLL_PUBLIC_URL": "https://console.internal:3443",
}

func setEnrolmentEnv(t *testing.T) {
	t.Helper()
	for k, v := range enrolmentEnv {
		t.Setenv(k, v)
	}
}

// A console with none of it set starts, with enrolment off.
//
// This is the state every deployment is in today, and it has to keep working:
// K7a must not make an existing console refuse to boot because it has not been
// told about a signer yet.
func TestAConsoleWithoutEnrolmentStarts(t *testing.T) {
	setConsoleEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a console with no enrolment settings was refused: %v", err)
	}
	if cfg.EnrollmentEnabled() {
		t.Error("enrolment reports enabled with nothing configured")
	}
}

// A console with all of it set starts, with enrolment on.
func TestACompleteEnrolmentConfigurationIsAccepted(t *testing.T) {
	setConsoleEnv(t)
	setEnrolmentEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a complete enrolment configuration was refused: %v", err)
	}
	if !cfg.EnrollmentEnabled() {
		t.Error("enrolment reports disabled with everything configured")
	}
}

// Every one-setting-missing case is refused, and the message names the setting.
//
// # Why each of these is worth refusing rather than tolerating
//
// None of them fails at startup on its own. A signer address with no client
// certificate produces a TLS handshake failure that reads as "signer
// unreachable". A listener with no key does not listen, and nothing says so —
// the console serves its normal port perfectly. Every one of them is discovered
// by whoever first needs a certificate, which is the worst moment to find out
// that somebody mistyped a path three deployments ago.
func TestHalfConfiguredEnrolmentIsRefused(t *testing.T) {
	for missing := range enrolmentEnv {
		t.Run("without "+missing, func(t *testing.T) {
			setConsoleEnv(t)
			setEnrolmentEnv(t)
			t.Setenv(missing, "")

			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv accepted enrolment with no %s", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s: %v", missing, err)
			}
		})
	}
}

// The refusal names every missing setting, not just the first one found.
//
// An operator configuring this from scratch would otherwise restart the console
// once per variable, which is how a five-minute task becomes an afternoon.
func TestTheRefusalNamesEveryMissingSetting(t *testing.T) {
	setConsoleEnv(t)
	t.Setenv("ATL_SIGNER_ADDR", enrolmentEnv["ATL_SIGNER_ADDR"])

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a signer address alone was accepted")
	}
	for name := range enrolmentEnv {
		if name == "ATL_SIGNER_ADDR" {
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not name %s: %v", name, err)
		}
	}
	// And it does not name the one that IS set, which would send somebody
	// looking at the wrong line.
	if strings.Contains(err.Error(), "ATL_SIGNER_ADDR is unset") {
		t.Errorf("the error names a setting that is present: %v", err)
	}
}

// CONSOLE_ENROLL_CLIENT_CA is gone, and a console starts without it.
//
// # The history, because the shape is worth remembering
//
// It named one pool for the enrolment listener to verify every renewing machine
// against, and it was optional. buildEnrollListener set ClientCAs only when it
// was non-empty, and Go's VerifyClientCertIfGiven with a nil ClientCAs verifies
// against the SYSTEM roots, where no caller certificate chains. So a console
// started from the Makefile's own CONSOLE_ENROLL_ENV — which did not set it —
// advertised enrolment as fully configured and refused every renewal inside the
// handshake, before handleRenew ran.
//
// **Every renewal test passed throughout, because the fixture set the variable
// the product did not require.** That is the shape to watch for: a setting only
// the test supplies is not configuration.
//
// Making it required fixed that. What it could never fix is one pool against
// many authorities, which per-organisation signers made the ordinary case — so
// the verification moved into handleRenew, where the organisation is known, and
// the setting had nothing left to name.
func TestEnrolmentDoesNotNeedAClientCASetting(t *testing.T) {
	setConsoleEnv(t)
	setEnrolmentEnv(t)
	t.Setenv("CONSOLE_ENROLL_CLIENT_CA", "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("a console was refused for want of a setting nothing reads: %v", err)
	}
	if !cfg.EnrollmentEnabled() {
		t.Error("enrolment is off without a setting it no longer uses")
	}
}

// And a console that still has it set refuses to start.
//
// Booting with it present and unread is the failure worth avoiding: whoever set
// it did so to say which authority may renew, and an inert variable leaves them
// believing a restriction is in force that nothing applies. cmd/server takes
// the same position on its retired authorization settings.
func TestAConsoleStillCarryingTheRetiredClientCASettingIsRefused(t *testing.T) {
	setConsoleEnv(t)
	setEnrolmentEnv(t)
	t.Setenv("CONSOLE_ENROLL_CLIENT_CA", "/certs/ca.crt")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("a console started with a retired setting still in its environment")
	}
	if !strings.Contains(err.Error(), "CONSOLE_ENROLL_CLIENT_CA") {
		t.Errorf("the error does not name the retired setting: %v", err)
	}
	if !strings.Contains(err.Error(), "no longer read") {
		t.Errorf("the error does not say the setting is retired: %v", err)
	}
}

// The same for the public URL, which is what makes the printed enrolment
// command runnable. Without it the console enrols perfectly and prints an
// instruction nobody can follow.
func TestEnrolmentRequiresThePublicURL(t *testing.T) {
	setConsoleEnv(t)
	setEnrolmentEnv(t)
	t.Setenv("CONSOLE_ENROLL_PUBLIC_URL", "")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("enrolment started with no address to send anybody to")
	}
	if !strings.Contains(err.Error(), "CONSOLE_ENROLL_PUBLIC_URL") {
		t.Errorf("the error does not name the missing setting: %v", err)
	}
}

// And the field is actually read from the environment.
//
// It was declared, documented, and never populated by ConfigFromEnv — so it was
// permanently empty and no test noticed, because none of them looked at a value
// rather than at an error.
func TestThePublicURLIsReadFromTheEnvironment(t *testing.T) {
	setConsoleEnv(t)
	setEnrolmentEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.EnrollPublicURL != enrolmentEnv["CONSOLE_ENROLL_PUBLIC_URL"] {
		t.Errorf("EnrollPublicURL = %q, want %q — the field is not being read",
			cfg.EnrollPublicURL, enrolmentEnv["CONSOLE_ENROLL_PUBLIC_URL"])
	}
}
