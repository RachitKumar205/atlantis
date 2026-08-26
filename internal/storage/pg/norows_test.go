package pg

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// A no-rows error from this adapter is visible to runtime.IsNoRows.
//
// A local sentinel carrying the runtime's message closes both routes at once:
//
//	var errNoRowsRuntimeAlias = errors.New("no rows")
//	return fmt.Errorf("%w: %s", errNoRowsRuntimeAlias, err)
//
// errors.Is compares by identity, so a same-message sentinel in another package
// never matches, and the wrap rewrites the message to "no rows: no rows in
// result set", which IsNoRows' string fallbacks — looking for exactly "no rows
// in result set" — also miss.
//
// handleGet turns IsNoRows into runtime.ErrNotFound, so a Get for a missing row
// then surfaces a raw driver error.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/storage/pg/ -run NoRowsFromThisAdapter -v
func TestNoRowsFromThisAdapterIsVisibleToTheRuntime(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the real adapter")
	}
	ctx := context.Background()
	pool, err := New(ctx, Config{URL: url, MaxConns: 2, MinConns: 1,
		MaxConnIdleTime: time.Minute, MaxConnLifetime: time.Hour, HealthCheckPeriod: time.Minute})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Through the real pgxRow.Scan, not a hand-built wrap. Constructing the
	// error here proves only that runtime.ErrNoRows behaves like a sentinel;
	// which sentinel the adapter wraps shows up only by calling it.
	var x int
	scanErr := pool.QueryRow(ctx, "SELECT 1 WHERE false").Scan(&x)
	if scanErr == nil {
		t.Fatal("expected an error scanning an empty result")
	}
	if !runtime.IsNoRows(scanErr) {
		t.Errorf("runtime.IsNoRows(%q) = false. Every no-rows this adapter "+
			"produces is reported as an unknown error instead of NotFound — "+
			"handleGet turns IsNoRows into runtime.ErrNotFound, so a Get for a "+
			"missing row surfaces a raw driver error.", scanErr)
	}
	if !errors.Is(scanErr, runtime.ErrNoRows) {
		t.Errorf("%q does not chain to runtime.ErrNoRows; a package-local "+
			"sentinel with the same message is not the same error", scanErr)
	}

	// And it must not swallow unrelated errors.
	if runtime.IsNoRows(errors.New("connection reset by peer")) {
		t.Error("IsNoRows matched an unrelated error")
	}
}
