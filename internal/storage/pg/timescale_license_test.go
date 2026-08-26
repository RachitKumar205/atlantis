package pg

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The Timescale License forbids using TSL software to provide a
// database-as-a-service, and its "Value Added" exception requires that users be
// "contractually or technically prohibited from modifying the database schema
// via Data Definition Interfaces" — which is precisely what atlantis exists to
// permit. So a hosted atlantis on the Community build has no carve-out.
//
// Only hypertable functionality is used, and create_hypertable /
// set_chunk_time_interval are Apache-2.0 (timescaledb sql/ddl_api.sql), so the
// Apache build is sufficient. What remains is confirming it is the build
// running.
//
// The default package is Community. Apache means installing timescaledb-apache
// or building with -DAPACHE_ONLY=1, once per image rebuild, which a check
// enforces and a runbook note does not.

func TestRequireApacheTimescale(t *testing.T) {
	for _, tc := range []struct {
		edition TimescaleEdition
		wantErr bool
		why     string
	}{
		{TimescaleApache, false, "the Apache build may be offered as a service"},
		{TimescaleAbsent, false, "no TimescaleDB means no Timescale License to satisfy"},
		{TimescaleCommunity, true, "the Community build may not be offered as a service"},
		{TimescaleEdition("something-new"), true,
			"an unrecognised value must not be assumed safe; a future build " +
				"reporting something new has to be looked at"},
	} {
		err := RequireApacheTimescale(tc.edition)
		if (err != nil) != tc.wantErr {
			t.Errorf("RequireApacheTimescale(%q) error = %v, want error = %v — %s",
				tc.edition, err, tc.wantErr, tc.why)
		}
		if tc.wantErr && !errors.Is(err, ErrTimescaleNotApache) {
			t.Errorf("RequireApacheTimescale(%q) does not wrap the sentinel, so a "+
				"caller cannot distinguish it from an unrelated failure", tc.edition)
		}
	}

	// The refusal names what to install, since it fires at deploy time.
	err := RequireApacheTimescale(TimescaleCommunity)
	for _, want := range []string{"APACHE_ONLY", "timescaledb-apache"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say how to fix it (missing %q): %v", want, err)
		}
	}
}

// Against a real database, because the detection reads a Postgres setting and
// the string it returns is the whole contract.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/storage/pg/ -run TimescaleEditionAgainstPostgres -v
func TestDetectTimescaleEditionAgainstPostgres(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to detect the edition of a real database")
	}
	ctx := context.Background()
	pool, err := New(ctx, Config{
		URL: url, MaxConns: 2, MinConns: 1,
		MaxConnIdleTime: time.Minute, MaxConnLifetime: time.Hour,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	edition, err := DetectTimescaleEdition(ctx, pool)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	t.Logf("this database reports timescaledb.license = %q", edition)

	switch edition {
	case TimescaleApache, TimescaleCommunity, TimescaleAbsent:
		// All three are legitimate answers; which one is a deployment fact.
	default:
		t.Errorf("unrecognised edition %q. Not a failure of this database, but it "+
			"must be classified before a hosted deployment relies on the check",
			edition)
	}

	// Whatever the answer, RequireApacheTimescale must agree with it. A
	// detector that reports one thing and a gate that decides on another is
	// worse than neither.
	gateErr := RequireApacheTimescale(edition)
	if edition == TimescaleCommunity && gateErr == nil {
		t.Error("the Community build was detected and the gate allowed it")
	}
	if edition == TimescaleApache && gateErr != nil {
		t.Errorf("the Apache build was detected and the gate refused it: %v", gateErr)
	}
}

// A database with no TimescaleDB must report absent rather than erroring — most
// deployments have no hypertables at all, and a spurious startup failure there
// would be worse than the problem being guarded against.
func TestDetectTimescaleEditionHandlesAbsentExtension(t *testing.T) {
	got, err := classifyLicenseError(errors.New(
		`ERROR: unrecognized configuration parameter "timescaledb.license" (SQLSTATE 42704)`))
	if err != nil {
		t.Fatalf("an absent extension was reported as an error: %v", err)
	}
	if got != TimescaleAbsent {
		t.Errorf("edition = %q, want absent", got)
	}

	// An unrelated failure must NOT be read as "absent" — that would report a
	// broken connection as a satisfied licence check.
	if _, err := classifyLicenseError(errors.New("connection refused")); err == nil {
		t.Error("an unrelated error was treated as an absent extension, which would " +
			"turn a connection failure into a passing licence check")
	}
}
