package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Boot the real run() and watch what it does about tenant isolation.
//
// # Why this exists when TestPartitionGate already passes
//
// TestPartitionGate proves the DECISION: given problems and require=true, an
// error comes back. The AST test proves the call sites exist and return on the
// error. Neither reads the ARGUMENTS, and a review found six mutations that
// survive the whole suite because of it — every one the same shape, a correct
// function handed the wrong value:
//
//	partitionGate(policyProblems, perr, cfg.RequireTenantIsolation, partitionedTables)
//	  cfg.RequireTenantIsolation -> false    SURVIVES
//	  policyProblems             -> nil      SURVIVES
//
// Both are caught here, because the only input is a database and an
// environment, and the only output is whether the process starts.
//
// # Why not a fifth AST rule
//
// That loop has run four times. Each round hardened the source rules against
// the escape the previous review demonstrated, and each round a review found
// another. The limit is structural: a rule strict enough to catch
// `if false && cfg.RequireTenantIsolation && ...` also rejects legitimate
// compound conditions. Reading the source cannot answer a question about
// values. Running the thing can.
func TestServerRefusesToBootWhenAPartitionedTableHasNoPolicy(t *testing.T) {
	// The child half. Boot once, say what happened, exit.
	//
	// A subprocess per case rather than three calls in one process, because
	// run() registers Prometheus collectors on the default registry and the
	// second call panics with "duplicate metrics collector registration
	// attempted". That is a property of booting a server, not a thing to work
	// around by making the server less like itself.
	if os.Getenv(bootChildEnv) != "" {
		bootChild()
		return
	}

	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to boot the server against a database")
	}

	// Its own database, not the shared test one.
	//
	// A booted server is not a passive reader. It starts the job scheduler,
	// which elects a single evaluator through a session advisory lock and
	// enqueues due work — so a boot here consumed the fire that
	// TestScheduleFiresAndReachesItsHandler in package jobs was waiting for,
	// and that test failed only when the two packages ran together. Every
	// other shared table has the same exposure.
	//
	// Nothing about the gate needs the shared database, so it does not use it.
	// A private one also removes the save-and-restore dance around
	// atlantis.ir_checkpoint, which was a shared-state hazard of its own.
	dsn := bootDatabase(t, adminDSN)

	for _, tc := range []struct {
		name        string
		require     string
		withPolicy  bool
		wantRefusal bool
		why         string
	}{
		{
			name: "declared, no policy, enforcement on", require: "true",
			withPolicy: false, wantRefusal: true,
			why: "the table serves every tenant's rows to every caller and the " +
				"operator asked atlantis not to start in that state",
		},
		{
			// Kills `cfg.RequireTenantIsolation -> false`. If the gate is handed a
			// constant instead of the config value, this case refuses too and the
			// server becomes unstartable on the default configuration.
			name: "declared, no policy, enforcement off", require: "false",
			withPolicy: false, wantRefusal: false,
			why: "ATL_REQUIRE_TENANT_ISOLATION defaults false and downgrades this " +
				"to a warning; refusing anyway would break every existing deployment",
		},
		{
			// Kills `policyProblems -> nil`, from the other side: if the gate is
			// handed nil it can never refuse, and the first case would be the only
			// one failing. This one proves the value is real rather than constant.
			name: "declared, policy enforced, enforcement on", require: "true",
			withPolicy: true, wantRefusal: false,
			why: "the table carries an enforced policy, which is the whole point " +
				"of the check passing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootFixture(t, dsn, tc.withPolicy)

			// Boot as a NON-SUPERUSER, which is what production looks like and
			// what this test needs to reach the question it is asking.
			//
			// `atlantis` is a superuser on a development database, and the boot
			// sequence refuses a superuser outright — every policy is inert
			// against one, which is a worse condition than a missing policy and is
			// checked first. Run as the default role, all three cases below refuse
			// for that reason and the policy gate is never reached, so the test
			// would pass while proving nothing about it.
			probeDSN := bootProbeRole(t, dsn)
			out := bootOnce(t, probeDSN, tc.require)

			refused := strings.HasPrefix(out, bootRefused)
			switch {
			case tc.wantRefusal && !refused:
				t.Errorf("the server started. It should have refused: %s.\nchild said: %s",
					tc.why, out)
			case !tc.wantRefusal && refused:
				t.Errorf("the server refused to start, and should not have: %s.\nchild said: %s",
					tc.why, out)
			}

			// A refusal has to name the entity. "tenant isolation check failed"
			// sends an operator to read source at the worst possible moment.
			if tc.wantRefusal && refused && !strings.Contains(out, "bootgate.Doc") {
				t.Errorf("the refusal does not name the offending entity, so an "+
					"operator cannot tell which table to look at: %s", out)
			}
		})
	}
}

const (
	bootChildEnv = "ATL_BOOT_GATE_CHILD"
	bootRefused  = "REFUSED: "
	bootStarted  = "STARTED"
)

// bootChild runs in the subprocess: boot once, report, exit.
//
// A refusal returns from run() immediately. A pass runs on to Serve and blocks,
// so the deadline is how "it got past the gate" is observed — what comes back
// then is the context's error, not the policy's.
func bootChild() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println(bootRefused + "loadConfig: " + err.Error())
		return
	}
	adminPolicy, err := authz.AdminPolicy()
	if err != nil {
		fmt.Println(bootRefused + "admin policy: " + err.Error())
		return
	}
	log, ring := buildLogger(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = run(ctx, cfg, log, ring, adminPolicy)
	switch {
	case err == nil,
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		fmt.Println(bootStarted)
	default:
		fmt.Println(bootRefused + err.Error())
	}
}

// bootOnce re-executes this test binary as a child that boots the server once,
// and returns the single line it printed.
func bootOnce(t *testing.T, pgURL, require string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestServerRefusesToBootWhenAPartitionedTableHasNoPolicy$",
		"-test.timeout", "90s")
	cmd.Env = append(os.Environ(),
		bootChildEnv+"=1",
		"PG_URL="+pgURL,
		"ATL_REQUIRE_TENANT_ISOLATION="+require,
		// Port 0 on loopback: the health server binds before the gate, and a
		// fixed port would collide with a real server or a parallel run.
		"HEALTH_LISTEN=127.0.0.1:0",
		"GRPC_LISTEN=127.0.0.1:0",
		"AUTO_MIGRATE=false",
		"ATL_JOBS_WORKER_ENABLED=false",
		"ATL_JOBS_DISPATCHER_ENABLED=false",
		"ATL_BACKFILL_WORKER_ENABLED=false",
		// The server logs JSON to stderr; keep it out of the answer channel.
		"LOG_LEVEL=error",
	)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// A non-zero exit is itself information — but only if the child said
		// nothing, since `go test` also exits non-zero on a failed assertion.
		if stdout.Len() == 0 {
			t.Fatalf("the boot child produced no verdict and exited with %v", err)
		}
	}
	// From the verdict marker to the end, not just its first line: a refusal
	// names the offending entities on continuation lines, and returning only
	// the first would hide exactly the part worth asserting on.
	out := stdout.String()
	if i := strings.Index(out, bootRefused); i >= 0 {
		return strings.TrimSpace(out[i:])
	}
	if strings.Contains(out, bootStarted) {
		return bootStarted
	}
	t.Fatalf("the boot child printed no verdict line:\n%s", out)
	return ""
}

// bootDatabase creates a private database, migrates it, and returns its DSN.
//
// Migrating here rather than letting the child do it with AUTO_MIGRATE keeps
// the children identical to a production boot — AUTO_MIGRATE defaults false and
// a server that migrates its own database on start is a different thing from
// one that finds it ready.
func bootDatabase(t *testing.T, adminDSN string) string {
	t.Helper()
	const dbName = "atlantis_bootgate"

	drop := func() {
		pgcatalog.Exec(t, adminDSN,
			// Terminate first: a child that has not fully exited still holds
			// connections, and DROP DATABASE refuses while any remain.
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '`+dbName+`'`,
			`DROP DATABASE IF EXISTS `+dbName)
	}
	drop()
	t.Cleanup(drop)
	pgcatalog.Exec(t, adminDSN, `CREATE DATABASE `+dbName)

	// Swap the database component of the DSN, leaving credentials and options.
	dsn := adminDSN
	if i := strings.LastIndex(dsn, "/atlantis?"); i >= 0 {
		dsn = dsn[:i] + "/" + dbName + dsn[i+len("/atlantis"):]
	} else {
		t.Fatalf("cannot derive a DSN for %s from %q — expected it to end in /atlantis?...", dbName, adminDSN)
	}

	// Extensions are per-database, so a fresh one has none of them and the pool
	// fails on `register pgvector: vector type not found` long before the gate.
	// The shared test database carries these four; a private one has to be told.
	pgcatalog.Exec(t, dsn,
		`CREATE EXTENSION IF NOT EXISTS citext`,
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	)

	if err := runAutoMigrate(dsn, "../../migrations", quietBootLogger()); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}
	return dsn
}

func quietBootLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// bootProbeRole creates a role the boot sequence will accept — no superuser, no
// BYPASSRLS — grants it what a running server touches, and returns a DSN for it.
//
// The grants are broad on purpose. Narrowing them to the exact set run() uses
// would make this test fail whenever the server starts reading a new table,
// which tells you nothing about tenant isolation and everything about the
// fixture. The one privilege that matters here is the one deliberately NOT
// granted: this role cannot see through row-level security.
func bootProbeRole(t *testing.T, dsn string) string {
	t.Helper()
	drop := func() {
		pgcatalog.Exec(t, dsn,
			`DROP OWNED BY bootgate_probe`,
			`DROP ROLE IF EXISTS bootgate_probe`)
	}
	drop()
	t.Cleanup(drop)

	pgcatalog.Exec(t, dsn,
		`CREATE ROLE bootgate_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA atlantis TO bootgate_probe`,
		`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA atlantis TO bootgate_probe`,
		`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA atlantis TO bootgate_probe`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA atlantis TO bootgate_probe`,
	)
	return strings.Replace(dsn, "//atlantis:atlantis@", "//bootgate_probe:probe@", 1)
}

// bootFixture leaves the database describing one partitioned entity, with or
// without an enforced policy on its table, and restores the IR checkpoint
// afterwards.
func bootFixture(t *testing.T, dsn string, withPolicy bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	drop := func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.bootgate_doc CASCADE`)
	}
	drop()
	t.Cleanup(drop)

	if _, err := pool.Exec(ctx, `
CREATE TABLE atlantis.bootgate_doc (
  id     bigint PRIMARY KEY,
  tenant text NOT NULL,
  body   text
)`); err != nil {
		t.Fatalf("fixture table: %v", err)
	}
	if withPolicy {
		// The shape the emitter produces: a RESTRICTIVE boundary that ANDs with
		// everything, plus the replaceable permissive grant without which a
		// restrictive-only table admits nothing at all.
		if _, err := pool.Exec(ctx, `
ALTER TABLE atlantis.bootgate_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.bootgate_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY bootgate_doc_tenant_isolation ON atlantis.bootgate_doc AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY bootgate_doc_default_access ON atlantis.bootgate_doc AS PERMISSIVE
  USING (true) WITH CHECK (true);`); err != nil {
			t.Fatalf("fixture policy: %v", err)
		}
	}

	ir := &dsl.IR{Entities: []dsl.Entity{{
		Name: "Doc", Namespace: "bootgate", PartitionField: "tenant",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}}}
	// Without this every field carries proto number 0 and registration fails
	// with `conflicting fields: "tenant" with "id"` long before the gate. A real
	// checkpoint always has them; a hand-built IR does not, and the difference
	// is invisible until the server tries to serve it.
	codegen.AssignProtoNumbers(nil, ir)
	raw, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("marshal ir: %v", err)
	}
	// No save-and-restore: this database belongs to this test and is dropped
	// after it.
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by) VALUES (1, $1, 'boot-gate-test')
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir`, raw); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
}
