package atltransport

import (
	"strings"
	"testing"
)

// Credentials refuses an incomplete environment, and names what is missing.
//
// This is the SDK a customer links against, so the error is the entire
// experience of getting it wrong. It used to return insecure credentials when
// ATL_TLS_CERT was unset, which produced a working *object* and a failure two
// layers later, at a handshake, in a message about neither certificates nor
// environment variables.
//
// Each variable is cleared separately. A guard written as `if certPath == ""`
// would pass a test that only cleared ATL_TLS_CERT while still handing back
// credentials with no key or no trust root.
func TestCredentialsRequiresEveryVariable(t *testing.T) {
	for _, missing := range []string{"ATL_TLS_CERT", "ATL_TLS_KEY", "ATL_TLS_CA"} {
		t.Run("without "+missing, func(t *testing.T) {
			setTLSEnv(t)
			t.Setenv(missing, "")

			creds, err := Credentials()
			if err == nil {
				t.Fatalf("Credentials succeeded with no %s, returning %T. atlantis "+
					"requires a client certificate on every connection, so this "+
					"cannot produce a usable channel", missing, creds)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s, so a caller cannot tell which "+
					"variable to set: %v", missing, err)
			}
		})
	}
}

// An environment with all three set gets past the check and fails on the files
// instead — which is the next real step, not a configuration complaint.
//
// The paths below do not exist, so this asserts WHICH failure happens: the
// error must be about loading the certificate, not about the environment. Pass
// it and the guard is not simply refusing everything.
func TestCredentialsAcceptsACompleteEnvironment(t *testing.T) {
	setTLSEnv(t)

	_, err := Credentials()
	if err == nil {
		t.Fatal("Credentials succeeded with paths that do not exist")
	}
	if strings.Contains(err.Error(), "not set") {
		t.Errorf("a complete environment was reported as incomplete, so the guard "+
			"refuses configurations that are correct: %v", err)
	}
	if !strings.Contains(err.Error(), "load client cert") {
		t.Errorf("expected the failure to come from reading the certificate "+
			"files, got: %v", err)
	}
}

func setTLSEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ATL_TLS_CERT", "/nonexistent/client.crt")
	t.Setenv("ATL_TLS_KEY", "/nonexistent/client.key")
	t.Setenv("ATL_TLS_CA", "/nonexistent/ca.crt")
}
