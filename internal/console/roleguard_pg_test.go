package console

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
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

	// Real certificates, because New dials atlantis before it opens the store.
	// With unreadable ones it fails on the cert and never reaches the gate —
	// which the first version of this test did, and its own content assertion
	// is what caught it. grpc.NewClient is lazy, so nothing has to be listening.
	pki := testpki.New(t, t.TempDir())
	cert, key := pki.ClientCert(t, "atlantis-console")

	_, err := New(Config{
		PGURL:         dsn,
		ATLEndpoint:   "127.0.0.1:1",
		SessionSecret: strings.Repeat("k", 32),
		ATLTLSCert:    cert,
		ATLTLSKey:     key,
		ATLTLSCA:      pki.CAFile,
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
