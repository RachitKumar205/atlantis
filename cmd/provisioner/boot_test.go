package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// What the provisioner refuses to start on, asserted from the outside.
//
// run() is main with the exits taken out for exactly this reason: a startup
// guard that only ever calls os.Exit can be asserted on by a subprocess test at
// best, and not at all from inside the package. cmd/signer and cmd/server are
// split the same way.
//
// These are hermetic on purpose — no database, no cluster, no ATLANTIS_TEST_*
// guard — because the tests that skip themselves are the tests nobody notices
// have stopped running. cmd/signer's suite is hermetic for the same reason, and
// it still went its entire life without CI compiling it once.

// bootEnv is a complete configuration whose database is deliberately
// unreachable: port 1 on loopback refuses immediately, so a test asserting on
// the connection error is fast and does not depend on a timeout.
var bootEnv = map[string]string{
	"CLOUD_PG_URL":     "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
	"CONSOLE_PG_URL":   "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable",
	"CONSOLE_DATA_KEY": "a-base64-keyset",
	"CLOUD_AUDIENCE":   "http://localhost:3000",

	"PROVISIONER_EXTERNAL_HOST":  "atl-dev.test",
	"PROVISIONER_SERVER_IMAGE":   "atlantis-server:local",
	"PROVISIONER_SIGNER_IMAGE":   "atlantis-signer:local",
	"PROVISIONER_POSTGRES_IMAGE": "atlantis-pg:17.11",
	"PROVISIONER_MEMCACHED_ADDR": "memcached:11211",
}

// clearEnv blanks everything run() reads, so a value in the developer's own
// shell cannot make a test pass here and fail in CI.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CLOUD_PG_URL", "CONSOLE_PG_URL", "CONSOLE_DATA_KEY", "CLOUD_AUDIENCE",
		"PROVISIONER_EXTERNAL_HOST", "PROVISIONER_SERVER_IMAGE", "PROVISIONER_SIGNER_IMAGE",
		"PROVISIONER_POSTGRES_IMAGE", "PROVISIONER_MEMCACHED_ADDR",
		"PROVISIONER_NAME", "PROVISIONER_POLL_INTERVAL", "PROVISIONER_LEASE",
		"PROVISIONER_LEASE_HEARTBEAT", "PROVISIONER_RETRY_BASE", "PROVISIONER_RETRY_MAX",
		"PROVISIONER_HEALTH_LISTEN", "PROVISIONER_READY_TIMEOUT",
		"PROVISIONER_NAMESPACE_PREFIX", "PROVISIONER_STORAGE_CLASS", "PROVISIONER_PULL_POLICY",
		"PROVISIONER_POD_CIDR", "PROVISIONER_OPERATOR_NAMESPACE",
		"PROVISIONER_POSTGRES_STORAGE", "PROVISIONER_POSTGRES_INSTANCES",
	} {
		t.Setenv(k, "")
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// An unconfigured provisioner refuses, and names everything it needs.
//
// One error listing every missing setting rather than one per restart. The
// alternative is an operator learning about nine variables over nine restarts,
// which is how a five-minute task becomes an afternoon.
func TestAnUnconfiguredProvisionerRefusesAndSaysWhy(t *testing.T) {
	clearEnv(t)

	err := run(quietLogger())
	if err == nil {
		t.Fatal("a provisioner with no configuration at all started")
	}
	for name := range bootEnv {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %v", name, err)
		}
	}
}

// Every one-setting-missing case is refused, and the message names the setting.
//
// None of these fails at startup on its own. A missing image reference is a pod
// that never starts, a missing external host is a certificate whose SAN matches
// nothing, and a missing memcached address is an organisation that never
// becomes Ready — all of them discovered by whoever first creates an
// organisation, which is the worst moment to find out.
func TestEachMissingSettingIsRefusedByName(t *testing.T) {
	for missing := range bootEnv {
		t.Run("without "+missing, func(t *testing.T) {
			clearEnv(t)
			for k, v := range bootEnv {
				t.Setenv(k, v)
			}
			t.Setenv(missing, "")

			err := run(quietLogger())
			if err == nil {
				t.Fatalf("the provisioner started with no %s", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the refusal does not name %s: %v", missing, err)
			}
		})
	}
}

// A complete configuration gets past the settings and fails at the database.
//
// This is the half that stops the test above from passing vacuously: without
// it, a run() that refused everything unconditionally would satisfy every
// assertion in this file. The error naming the database is what proves the
// configuration was accepted and startup carried on.
func TestACompleteConfigurationReachesTheDatabase(t *testing.T) {
	clearEnv(t)
	for k, v := range bootEnv {
		t.Setenv(k, v)
	}

	err := run(quietLogger())
	if err == nil {
		t.Fatal("the provisioner started against a database that does not exist")
	}
	if !strings.Contains(err.Error(), "cloud schema") && !strings.Contains(err.Error(), "cloud db") {
		t.Errorf("the failure is not the database, so configuration may have been "+
			"refused for some other reason: %v", err)
	}
	for name := range bootEnv {
		if strings.Contains(err.Error(), name) {
			t.Errorf("the error names %s, which is set — the configuration was refused "+
				"when it should have been accepted: %v", name, err)
		}
	}
}

// A lease that could expire during the readiness wait is refused at startup.
//
// The condition it prevents: WaitReady burns the whole ReadyTimeout, the lease
// expires mid-wait, a second provisioner claims the organisation and starts
// from the top — and the first returns and stamps its result over the row,
// because MarkProvisioned is not guarded on claimed_by.
func TestALeaseThatCannotOutliveTheWaitIsRefused(t *testing.T) {
	clearEnv(t)
	for k, v := range bootEnv {
		t.Setenv(k, v)
	}
	t.Setenv("PROVISIONER_READY_TIMEOUT", "10m")
	t.Setenv("PROVISIONER_LEASE", "1m")

	err := run(quietLogger())
	if err == nil {
		t.Fatal("a provisioner whose lease expires mid-wait started")
	}
	if !strings.Contains(err.Error(), "PROVISIONER_LEASE") {
		t.Errorf("the refusal does not name the setting: %v", err)
	}
}
