package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// What the signer refuses to start with.
//
// # Why these are startup errors rather than request-time ones
//
// This process holds a certificate authority's private key and mints caller
// identities with it. Every setting below is the difference between "who may
// ask" being enforced and not being enforced at all — and none of them
// announces itself at request time in a way anybody would notice: an absent
// client CA would simply mean every peer is admitted, and an empty allowlist
// would mean every common name is.
//
// The precedent is `PG_URL`. It used to be optional, and the comment above it
// claimed that failing to connect was fatal "in production posture" — true only
// of a DSN that was set and broken. A DSN that was *absent* silently reduced the
// signer to a reserved-name denylist, which is not a check on anything an
// operator registered. So the deployment with the least configuration had the
// fewest checks, which is exactly backwards.
//
// run() exists so these can be asserted from inside the package. main() calls it
// and exits; everything below calls it and reads the error.

// signerEnv is a complete, working configuration.
//
// Tests clear one variable each, so a new requirement is added here once rather
// than found one failing test at a time.
func signerEnv(t *testing.T) map[string]string {
	t.Helper()
	// The authority the signer ISSUES from.
	issuing := testpki.New(t, t.TempDir())
	// A second, separate authority: who may CALL it. Separate on purpose — see
	// loadClientCAs for what sharing one would open.
	clients := testpki.New(t, t.TempDir())

	return map[string]string{
		"CA_DIR":                    issuing.Dir,
		"SIGNER_CLIENT_CA":          clients.CAFile,
		"SIGNER_ALLOWED_CLIENT_CNS": "atlantis-console",
		"SIGNER_TLS_CERT":           clients.CertFile,
		"SIGNER_TLS_KEY":            clients.KeyFile,
		"PG_URL":                    "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
	}
}

func setSignerEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// quiet discards the signer's log lines. The error is the assertion, not the
// output.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSignerRefusesAWeakConfiguration(t *testing.T) {
	for _, tc := range []struct {
		missing string
		names   string
	}{
		{"SIGNER_CLIENT_CA", "SIGNER_CLIENT_CA"},
		{"SIGNER_ALLOWED_CLIENT_CNS", "SIGNER_ALLOWED_CLIENT_CNS"},
		{"PG_URL", "PG_URL"},
		{"SIGNER_TLS_CERT", "SIGNER_TLS_CERT"},
		{"SIGNER_TLS_KEY", "SIGNER_TLS_KEY"},
	} {
		t.Run("without "+tc.missing, func(t *testing.T) {
			env := signerEnv(t)
			setSignerEnv(t, env)
			t.Setenv(tc.missing, "")

			err := run(quiet())
			if err == nil {
				t.Fatalf("the signer started with no %s", tc.missing)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the error does not name %s: %v", tc.names, err)
			}
		})
	}
}

// An allowlist of only separators is empty, and empty must not mean everyone.
//
// `SIGNER_ALLOWED_CLIENT_CNS=","` is what a half-finished edit looks like. It is
// not the unset case, so a check that only tested for the empty string would
// pass it through — and the resulting allowlist admits every common name that
// chains to the client CA.
func TestAnAllowlistOfSeparatorsIsEmpty(t *testing.T) {
	for _, raw := range []string{",", " , ", "  "} {
		env := signerEnv(t)
		setSignerEnv(t, env)
		t.Setenv("SIGNER_ALLOWED_CLIENT_CNS", raw)

		if err := run(quiet()); err == nil {
			t.Fatalf("the signer started with SIGNER_ALLOWED_CLIENT_CNS=%q", raw)
		}
	}
}

// parseAllowedCNs keeps every name it is given, trimmed.
func TestTheAllowlistKeepsEveryNameItIsGiven(t *testing.T) {
	got, err := parseAllowedCNs(" atlantis-console , atlantis-provisioner ")
	if err != nil {
		t.Fatalf("parseAllowedCNs: %v", err)
	}
	if len(got) != 2 || !got["atlantis-console"] || !got["atlantis-provisioner"] {
		t.Errorf("allowlist = %v, want both names trimmed", got)
	}
}

// A client CA file that is not a certificate is refused, rather than producing
// an empty pool that verifies nothing.
//
// x509.CertPool.AppendCertsFromPEM reports whether it added anything and is
// very easy to ignore. Ignoring it here would leave a pool with no authorities
// in it, and `RequireAndVerifyClientCert` against an empty pool refuses every
// peer — so the signer would start and then refuse the console, which reads as
// "the console cannot reach the signer" rather than as a bad path.
func TestAClientCAThatIsNotACertificateIsRefused(t *testing.T) {
	env := signerEnv(t)
	setSignerEnv(t, env)

	notACert := t.TempDir() + "/empty.pem"
	if err := writeFile(notACert, "this is not a certificate\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIGNER_CLIENT_CA", notACert)

	err := run(quiet())
	if err == nil {
		t.Fatal("the signer started with a client CA containing no certificate")
	}
	if !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("the error does not say the file held no certificate: %v", err)
	}
}

// A complete configuration gets past every check above.
//
// Without this the suite would pass on a build that refused everything, which
// is the other way a startup gate goes wrong. It stops at the database, because
// PG_URL points nowhere — reaching that point means the certificate settings
// were all accepted.
func TestACompleteConfigurationReachesTheDatabase(t *testing.T) {
	env := signerEnv(t)
	setSignerEnv(t, env)

	err := run(quiet())
	if err == nil {
		t.Fatal("run returned no error, so it began serving with an unreachable database")
	}
	if !strings.Contains(err.Error(), "connect to PG") {
		t.Fatalf("the signer stopped before the database, so a certificate setting was refused: %v", err)
	}
}

// writeFile is a one-line helper the tests above share.
func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
