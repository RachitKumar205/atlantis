package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// TimescaleEdition is which TimescaleDB build a database is running, if any.
type TimescaleEdition string

const (
	// TimescaleAbsent means the extension is not installed. Nothing to decide.
	TimescaleAbsent TimescaleEdition = "absent"
	// TimescaleApache is the Apache 2.0 build. May be offered as a service.
	TimescaleApache TimescaleEdition = "apache"
	// TimescaleCommunity is the Timescale License build. Free to self-host,
	// and forbidden to offer as a database service.
	TimescaleCommunity TimescaleEdition = "timescale"
)

// ErrTimescaleNotApache is returned when a Community-edition build is found and
// the caller required Apache.
var ErrTimescaleNotApache = errors.New("pg: TimescaleDB is the Community (TSL) build")

// DetectTimescaleEdition reports which TimescaleDB build the database runs.
//
// Why this exists, rather than a note in a runbook: the DEFAULT TimescaleDB
// package is the Community build. Getting the Apache one requires deliberately
// installing timescaledb-apache or building with -DAPACHE_ONLY=1, so "we
// installed the right package" is a thing someone has to remember on every
// image rebuild — which is not a control.
//
// The stake is licensing, not correctness. The Timescale License forbids using
// TSL software to provide "database-as-a-service ... to third parties", and its
// Value Added exception requires that users be "contractually or technically
// prohibited from modifying the database schema via Data Definition
// Interfaces". atlantis exists to let users modify schema via DDL, so that
// exception cannot apply to a hosted atlantis by construction. A hosted product
// on the Community build has no carve-out to stand on.
//
// Self-hosting is unaffected: the TSL restricts offering the software as a
// service, not running it. So this reports rather than refuses, and refusing is
// the caller's decision — see RequireApacheTimescale.
func DetectTimescaleEdition(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) runtime.Row
}) (TimescaleEdition, error) {
	var setting string
	if err := q.QueryRow(ctx, "SHOW timescaledb.license").Scan(&setting); err != nil {
		return classifyLicenseError(err)
	}
	switch strings.TrimSpace(strings.ToLower(setting)) {
	case "apache":
		return TimescaleApache, nil
	case "timescale":
		return TimescaleCommunity, nil
	default:
		// An unknown value is not treated as safe. A future build reporting
		// something new must be looked at rather than assumed benign.
		return TimescaleEdition(setting), fmt.Errorf(
			"pg: unrecognised timescaledb.license value %q; treat as non-Apache until checked", setting)
	}
}

// RequireApacheTimescale returns an error unless the edition may be offered as
// a service.
//
// Absent counts as satisfied: no TimescaleDB means no Timescale License.
func RequireApacheTimescale(edition TimescaleEdition) error {
	switch edition {
	case TimescaleAbsent, TimescaleApache:
		return nil
	default:
		return fmt.Errorf("%w (timescaledb.license=%q). Offering this as a hosted "+
			"database service is not permitted by the Timescale License, and the "+
			"\"Value Added\" exception does not apply to a product whose purpose is "+
			"letting users modify schema via DDL. Rebuild with -DAPACHE_ONLY=1 or "+
			"install timescaledb-apache. Only hypertable functionality is used, and "+
			"that is Apache-2.0 licensed",
			ErrTimescaleNotApache, string(edition))
	}
}

// classifyLicenseError separates "the extension is not installed" from "the
// query failed".
//
// The distinction is load-bearing and easy to get wrong in the dangerous
// direction: treating every error as absent would turn a refused connection, a
// permission problem or a timeout into a satisfied licence check. Most
// deployments genuinely have no TimescaleDB, so the absent path must be exact
// rather than a catch-all.
func classifyLicenseError(err error) (TimescaleEdition, error) {
	if strings.Contains(strings.ToLower(err.Error()), "unrecognized configuration parameter") {
		return TimescaleAbsent, nil
	}
	return "", fmt.Errorf("pg: detect TimescaleDB edition: %w", err)
}
