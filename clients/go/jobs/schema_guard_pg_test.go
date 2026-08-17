package jobs

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The guard that stops this SDK running against a server older than itself.
//
// clients/go is versioned independently of the atlantis server, so an app can
// upgrade the SDK past the release that applied migration 0028. The skew has
// no early symptom: claims, leases and completions all work, because only the
// terminal-failure path names owner_caller. The first sign is a job exhausting
// its retries and wedging — ReportFailure errors, the worker logs it at Warn,
// and the row sits status='running' with attempts == max_retries, which every
// later claim excludes. SweepExhaustedToDLQ fails on the same column.
//
// Driven against a real catalogue because that is what the guard reads. A unit
// test over a fake would assert the query I wrote rather than the one Postgres
// answers.

// guardDB creates a THROWAWAY database and returns a pool on it.
//
// Its own database, not the shared one. The guard is hardcoded to the
// `atlantis` schema, so a fixture for it has to create and drop that schema —
// and doing that in the shared ATLANTIS_TEST_PG database would delete the
// migrated schema every other test in the repo runs against. The first draft
// of this file did exactly that.
func guardDB(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the SDK schema guard")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup, NOT defer, and registered before the drop below.
	//
	// `defer admin.Close()` closes the pool when guardDB RETURNS, which is long
	// before the test ends — so the cleanup drop then ran against a closed pool,
	// discarded the "closed pool" error like every other error there, and left
	// the database behind. Three of them were sitting in the cluster before this
	// was noticed, one per test, growing on every run.
	//
	// t.Cleanup runs LIFO, so registering the close first makes it run LAST:
	// drop, then close.
	t.Cleanup(admin.Close)

	// WITH (FORCE) rather than pg_terminate_backend followed by a DROP.
	// pg_terminate_backend signals a backend, it does not wait for it to exit, so
	// the DROP that follows can still see the connection and fail — which is a
	// database left behind, and the next run inherits it.
	//
	// The same helper inside the repo is internal/testsupport/pgcatalog.
	// PrivateDatabase. This module cannot import it: clients/go is a separate
	// module, and internal/ is unreachable from outside the one that declares it.
	drop := func() {
		// Reported rather than discarded. A cleanup that cannot say it failed is
		// how the leak above went unseen.
		if _, err := admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("could not drop %s, so it is left behind and the next run "+
				"inherits it: %v", name, err)
		}
	}
	drop()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(drop)

	i := strings.LastIndex(adminDSN, "/atlantis?")
	if i < 0 {
		t.Skipf("cannot derive a DSN for %s from the configured one", name)
	}
	dsn := adminDSN[:i] + "/" + name + adminDSN[i+len("/atlantis"):]

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// withJobTables builds atlantis.jobs and atlantis.jobs_dead with or without
// owner_caller. Only information_schema matters to the guard, so the columns
// are the minimum that makes the tables real.
func withJobTables(t *testing.T, pool *pgxpool.Pool, withOwner bool) {
	t.Helper()
	owner := ""
	if withOwner {
		owner = ", owner_caller TEXT NOT NULL DEFAULT ''"
	}
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS atlantis`,
		`CREATE TABLE atlantis.jobs (id BIGSERIAL PRIMARY KEY` + owner + `)`,
		`CREATE TABLE atlantis.jobs_dead (id BIGINT PRIMARY KEY` + owner + `)`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}

func TestRequireOwnerCallerColumnRefusesAnOlderServer(t *testing.T) {
	pool := guardDB(t, "atlantis_sdkguard_old")
	withJobTables(t, pool, false)

	err := requireOwnerCallerColumn(context.Background(), pool)
	if err == nil {
		t.Fatal("the guard passed against a database with no owner_caller. " +
			"The worker would start, run normally, and wedge the first job that " +
			"exhausted its retries — with one Warn line as the only trace.")
	}
	// The message has to name the fix: the operator seeing it has an SDK and a
	// server and no reason to suspect which of the two is wrong.
	for _, want := range []string{"0028", "older than this SDK", "upgrade the"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not tell the "+
				"operator what to do: %v", want, err)
		}
	}
}

func TestRequireOwnerCallerColumnAcceptsACurrentServer(t *testing.T) {
	pool := guardDB(t, "atlantis_sdkguard_new")
	withJobTables(t, pool, true)

	if err := requireOwnerCallerColumn(context.Background(), pool); err != nil {
		t.Errorf("the guard refused a database that HAS owner_caller on both "+
			"tables, so it would block every correctly-migrated deployment: %v", err)
	}
}

// TestRequireOwnerCallerColumnRefusesAPartialMigration covers the state a
// half-applied migration leaves. Counting both tables rather than checking one
// is what makes this detectable, and asserting it stops the count being
// loosened to `> 0` — MoveToDLQ writes to jobs_dead, so that is the half whose
// absence wedges.
func TestRequireOwnerCallerColumnRefusesAPartialMigration(t *testing.T) {
	pool := guardDB(t, "atlantis_sdkguard_partial")
	ctx := context.Background()
	withJobTables(t, pool, false)

	if _, err := pool.Exec(ctx,
		`ALTER TABLE atlantis.jobs ADD COLUMN owner_caller TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("add the column to one table: %v", err)
	}

	if err := requireOwnerCallerColumn(ctx, pool); err == nil {
		t.Error("the guard passed with owner_caller on jobs but not jobs_dead")
	}
}
