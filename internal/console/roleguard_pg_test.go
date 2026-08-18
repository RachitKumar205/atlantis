package console

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The gate is reached, on the real boot path.
//
// roleguard_test.go proves consoleRoleError ranks the postures correctly. That
// is a different claim from "New consults it", and the difference is the one
// this repo keeps paying for: the harness now connects as a clean role, so
// deleting the call from New breaks nothing there. Without this test the guard
// could be removed entirely and the suite would stay green.
//
// Env-gated like the other live-PG tests here.
func TestConsoleRefusesToStartOnARoleThatBypassesRLS(t *testing.T) {
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the console's role gate")
	}

	// The admin DSN deliberately: it is the superuser the developer's console
	// used to run as, which is the mistake being refused.
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_console_rolegate")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A real keyset, because it is now the first thing New checks.
	//
	// This used to be a real certificate, for the same reason: whatever New
	// validates first is what an incomplete Config fails on, and the test then
	// never reaches the gate it is about. Certificates left this function when
	// they became a property of an organisation rather than of the process, and
	// the keyring took over the position — so the hazard moved rather than
	// went away. The content assertion below is what catches it either way, and
	// it is what caught the certificate version.
	_, err := New(Config{
		PGURL:         dsn,
		DataKeyset:    testKeyset(t),
		SessionSecret: strings.Repeat("k", 32),
	}, nil, quiet)

	if err == nil {
		t.Fatal("the console started against a superuser role. Every " +
			"organisation policy would be attached and inert, and every " +
			"request would read every organisation's rows")
	}
	// Asserted on content: New has several ways to fail, and a test satisfied
	// by any error would pass while the role gate was deleted — the TLS paths
	// above do not exist, so the dial would fail on its own.
	if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("the console refused to start, but not because of the role. "+
			"This test would pass with the gate removed: %v", err)
	}
}
