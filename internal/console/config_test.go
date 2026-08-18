package console

import (
	"strings"
	"testing"
)

// setConsoleEnv sets the minimum ConfigFromEnv accepts. Tests about one missing
// variable start from a complete environment and clear that one, so a new
// requirement is added here rather than found one failing test at a time.
func setConsoleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONSOLE_PG_URL", "postgres://x")
	t.Setenv("CONSOLE_SESSION_SECRET", strings.Repeat("k", 32))
	t.Setenv("ATL_TLS_CERT", "/etc/atlantis/console.crt")
	t.Setenv("ATL_TLS_KEY", "/etc/atlantis/console.key")
	t.Setenv("ATL_TLS_CA", "/etc/atlantis/ca.crt")
	t.Setenv("CLOUD_ISSUER", "https://cloud.atlantis.dev")
	t.Setenv("CLOUD_AUDIENCE", "https://acme.console.atlantis.dev")
	t.Setenv("CLOUD_JWKS_URL", "https://cloud.atlantis.dev/.well-known/jwks.json")
}

// The console will not start without an issuer to trust.
//
// This is stricter than it may look, and the strictness is the point. The
// verifier compares iss and aud for exact equality, and an empty expected
// value means the check is skipped — so a console missing either one would not
// refuse to start, it would start and accept assertions from any issuer, for
// any console. Nothing downstream could notice: every request would carry a
// valid session, established from a token that verified.
func TestConfigFromEnvRequiresTheCloudIssuer(t *testing.T) {
	for _, missing := range []string{"CLOUD_ISSUER", "CLOUD_AUDIENCE", "CLOUD_JWKS_URL"} {
		t.Run("without "+missing, func(t *testing.T) {
			setConsoleEnv(t)
			t.Setenv(missing, "")

			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv accepted a console with no %s", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s: %v", missing, err)
			}
		})
	}
}

// The console will not start without its client certificate.
//
// ConfigFromEnv had no test of any kind, so every requirement it enforces —
// including the two that predate this one — was resting on nothing. The mTLS
// requirement in particular is what makes the console an authenticated caller
// rather than an anonymous one, and the harness in harness_test.go builds a
// Config literal, so it exercises none of this.
func TestConfigFromEnvRequiresEveryTLSVariable(t *testing.T) {
	for _, missing := range []string{"ATL_TLS_CERT", "ATL_TLS_KEY", "ATL_TLS_CA"} {
		t.Run("without "+missing, func(t *testing.T) {
			setConsoleEnv(t)
			t.Setenv(missing, "")

			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv accepted a config with no %s. The console "+
					"would start and then fail at the handshake, in a message "+
					"about neither certificates nor configuration", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name %s: %v", missing, err)
			}
			if !strings.Contains(err.Error(), "make dev-certs") {
				t.Errorf("the error does not name the command that produces the "+
					"certificates: %v", err)
			}
		})
	}
}

// The pre-existing requirements, now that something exercises this function.
func TestConfigFromEnvRequiresPGURLAndSessionSecret(t *testing.T) {
	t.Run("without CONSOLE_PG_URL", func(t *testing.T) {
		setConsoleEnv(t)
		t.Setenv("CONSOLE_PG_URL", "")
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("accepted a console with no database")
		}
	})
	t.Run("with a short session secret", func(t *testing.T) {
		setConsoleEnv(t)
		t.Setenv("CONSOLE_SESSION_SECRET", "tooshort")
		if _, err := ConfigFromEnv(); err == nil {
			t.Fatal("accepted a session secret under 32 characters, which is the " +
				"guard that stops a `changeme` placeholder signing real sessions")
		}
	})
}

// And a complete environment loads, with the values carried through.
//
// Without this, a ConfigFromEnv that returned an error unconditionally would
// pass every test above.
func TestConfigFromEnvAcceptsACompleteEnvironment(t *testing.T) {
	setConsoleEnv(t)
	t.Setenv("CONSOLE_LISTEN", ":4000")

	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv rejected a complete environment: %v", err)
	}
	if c.Listen != ":4000" {
		t.Errorf("Listen: got %q, want :4000", c.Listen)
	}
	if c.ATLTLSCert == "" || c.ATLTLSKey == "" || c.ATLTLSCA == "" {
		t.Errorf("TLS paths did not survive loading: cert=%q key=%q ca=%q",
			c.ATLTLSCert, c.ATLTLSKey, c.ATLTLSCA)
	}
}
