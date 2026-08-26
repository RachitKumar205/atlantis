package console

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/secrets"
)

// setConsoleEnv sets the minimum ConfigFromEnv accepts. Tests about one missing
// variable start from a complete environment and clear that one, so a new
// requirement is added here rather than found one failing test at a time.
func setConsoleEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONSOLE_PG_URL", "postgres://x")
	t.Setenv("CONSOLE_SESSION_SECRET", strings.Repeat("k", 32))
	t.Setenv("CONSOLE_DATA_KEY", testKeyset(t))
	t.Setenv("CLOUD_ISSUER", "https://cloud.atlantis.dev")
	t.Setenv("CLOUD_AUDIENCE", "https://acme.console.atlantis.dev")
	t.Setenv("CLOUD_JWKS_URL", "https://cloud.atlantis.dev/.well-known/jwks.json")
}

// The console will not start without an issuer to trust.
//
// The verifier compares iss and aud for exact equality, and an empty expected
// value skips the check, so a console missing either one starts and accepts
// assertions from any issuer for any console. Nothing downstream notices: every
// request carries a valid session, established from a token that verified.
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

// The console will not start without the key its organisations' credentials
// are sealed with.
//
// This replaced a test of ATL_TLS_CERT/KEY/CA, which no longer exist: a client
// certificate is now a property of an organisation rather than of the process,
// so there is no process-wide certificate left to demand. mTLS did not become
// optional — dialOrg has no insecure branch, and an organisation with no
// credentials is refused rather than dialled in the clear.
//
// What moved is where the failure lands. Without this keyset the console starts
// and then cannot decrypt a single organisation's certificate, so every page
// fails at the first request with an error about ciphertext rather than about
// configuration.
func TestConfigFromEnvRequiresTheDataKeyset(t *testing.T) {
	setConsoleEnv(t)
	t.Setenv("CONSOLE_DATA_KEY", "")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("ConfigFromEnv accepted a console with no CONSOLE_DATA_KEY")
	}
	if !strings.Contains(err.Error(), "CONSOLE_DATA_KEY") {
		t.Errorf("the error does not name the variable: %v", err)
	}
	if !strings.Contains(err.Error(), "make dev-data-key") {
		t.Errorf("the error does not name the command that produces one: %v", err)
	}
}

// testKeyset mints a real keyset, so the config tests exercise the value the
// console will actually be given rather than a placeholder that would pass a
// non-empty check and fail at first use.
func testKeyset(t *testing.T) string {
	t.Helper()
	k, err := secrets.NewKeyset()
	if err != nil {
		t.Fatalf("generate test keyset: %v", err)
	}
	return k
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
	if c.DataKeyset == "" {
		t.Errorf("CONSOLE_DATA_KEY did not survive loading")
	}
}
