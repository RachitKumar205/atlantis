package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto/sha256"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/authz"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The partition gate on the HOT-RELOAD path, driven through a running server.
//
// partitionGate has two call sites. TestServerRefusesToBootWhenAPartitionedTableHasNoPolicy
// covers the one at startup; this covers the one in the LISTEN/NOTIFY reload
// hook, which an AST tripwire can only show is present in the source, never
// that it fires.
//
// The case: a checkpoint adding `partition by` to an existing entity reaches a
// server that is already up, the reload turns on `partitioned` in the
// dispatcher for a table carrying no policy, and the observable signal — omit
// the tenant, get refused — goes on reporting healthy. Boot-time verification
// ran before the schema changed.
//
// The child logs at info, so both outcomes are distinguishable:
//
//	"schema reload: rebuild failed"   the hook refused; snapshot.Store is never
//	                                  reached, so the OLD schema keeps serving
//	"schema hot-reload complete"      the swap happened
//
// Asserting the refusal alone would pass against a server that refuses every
// reload, so the accepted case is driven too. One fixture, one policy
// difference, opposite verdicts.
func TestReloadRefusesASchemaTheDatabaseIsNotEnforcing(t *testing.T) {
	if os.Getenv(reloadChildEnv) != "" {
		reloadChild()
		return
	}
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reload gate")
	}

	cases := []struct {
		name       string
		withPolicy bool
		wantAccept bool
	}{{
		name:       "a partitioned table with no policy is refused",
		withPolicy: false,
		wantAccept: false,
	}, {
		name:       "a partitioned table carrying its policy is accepted",
		withPolicy: true,
		wantAccept: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := bootDatabase(t, adminDSN, "atlantis_reloadgate")
			probeDSN := reloadProbeRole(t, dsn)

			// Boot clean: one entity, no `partition by`, so the gate at startup
			// has nothing to object to and the child gets as far as listening.
			seedCheckpoint(t, dsn, reloadIR(false), "clean")
			reloadTable(t, dsn, tc.withPolicy)

			child, lines := startReloadChild(t, probeDSN)
			defer func() { _ = child.Process.Kill() }()

			if !awaitLine(t, lines, "schema listener: LISTEN active", 60*time.Second) {
				t.Fatal("the child never began listening, so nothing below tests the reload path")
			}

			// The change that turns on `partition by` at a running server. The
			// trigger on ir_checkpoint fires the NOTIFY.
			seedCheckpoint(t, dsn, reloadIR(true), "partitioned")

			accepted, refused := awaitVerdict(t, lines, 60*time.Second)
			// Logged so the transcript shows the reload path was reached and
			// which way it went. A pass with neither flag set would mean the
			// assertions below were vacuous.
			t.Logf("reload verdict: accepted=%v refused=%v", accepted, refused)
			if !accepted && !refused {
				t.Fatal("no verdict observed; the assertions below would pass for a " +
					"server that never reloaded at all")
			}
			switch {
			case tc.wantAccept && !accepted:
				t.Errorf("a partitioned table WITH an enforced policy was refused on "+
					"reload. The gate is refusing changes the database does enforce, "+
					"which is how a deployment ends up turning it off. refused=%v",
					refused)
			case !tc.wantAccept && accepted:
				t.Error("a partitioned table with NO policy was accepted on reload. " +
					"The dispatcher now believes the table is tenant-isolated, every " +
					"read returns every tenant's rows, and the health surface an " +
					"operator watches reports nothing wrong.")
			}
		})
	}
}

const reloadChildEnv = "ATL_RELOAD_GATE_CHILD"

// reloadChild boots a server and stays up long enough to receive a reload.
//
// Unlike bootChild it prints no verdict: what is being observed is the running
// server's log stream, not its exit.
func reloadChild() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reload child: loadConfig:", err)
		return
	}
	adminPolicy, err := authz.AdminPolicy()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reload child: admin policy:", err)
		return
	}
	log, ring := buildLogger(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := run(ctx, cfg, log, ring, adminPolicy); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintln(os.Stderr, "reload child: run:", err)
	}
}

// startReloadChild re-executes this binary as a server and streams its stderr.
func startReloadChild(t *testing.T, pgURL string) (*exec.Cmd, <-chan string) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run", "^TestReloadRefusesASchemaTheDatabaseIsNotEnforcing$",
		"-test.timeout", "120s")
	// mTLS is required to boot; see the note in bootOnce.
	certFile, keyFile, caFile := writeServerPKI(t, t.TempDir())
	cmd.Env = append(os.Environ(),
		reloadChildEnv+"=1",
		"PG_URL="+pgURL,
		"TLS_CERT_FILE="+certFile,
		"TLS_KEY_FILE="+keyFile,
		"TLS_CA_FILE="+caFile,
		// The refusal only fires when isolation is required, matching boot.
		"ATL_REQUIRE_TENANT_ISOLATION=true",
		"HEALTH_LISTEN=127.0.0.1:0",
		"GRPC_LISTEN=127.0.0.1:0",
		"AUTO_MIGRATE=false",
		"ATL_JOBS_WORKER_ENABLED=false",
		"ATL_JOBS_DISPATCHER_ENABLED=false",
		"ATL_BACKFILL_WORKER_ENABLED=false",
		// info, not error: "schema hot-reload complete" is the accepted-case
		// signal and is logged at info. At error level the two outcomes would
		// be indistinguishable from the absence of a message.
		"LOG_LEVEL=info",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	// stdout too. The server logs to stderr, but a child that dies before the
	// logger exists says so on stdout — and a test that reads only one of the
	// two reports "the child said nothing" when the child said plenty.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("child: %s", cmd.Path)

	lines := make(chan string, 512)
	var wg sync.WaitGroup
	for _, r := range []io.Reader{stderr, stdout} {
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				select {
				case lines <- sc.Text():
				default: // a full buffer means nobody is reading any more
				}
			}
		}(r)
	}
	go func() {
		wg.Wait()
		close(lines)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, lines
}

// awaitLine reads until the wanted substring appears, and on failure reports
// everything it did see.
//
// The transcript is the point. "the child never began listening" with nothing
// else is a test that has noticed a problem and thrown away the only evidence
// of what it was — and the child's own log lines name it exactly.
func awaitLine(t *testing.T, lines <-chan string, want string, budget time.Duration) bool {
	t.Helper()
	var seen []string
	deadline := time.After(budget)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Logf("the child's output ended after %d line(s):\n%s",
					len(seen), strings.Join(seen, "\n"))
				return false
			}
			seen = append(seen, line)
			if strings.Contains(line, want) {
				return true
			}
		case <-deadline:
			t.Logf("waited %s for %q; the child said:\n%s",
				budget, want, strings.Join(seen, "\n"))
			return false
		}
	}
}

// awaitVerdict reads until the server reports one outcome or the other.
func awaitVerdict(t *testing.T, lines <-chan string, budget time.Duration) (accepted, refused bool) {
	t.Helper()
	deadline := time.After(budget)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return false, false
			}
			switch {
			case strings.Contains(line, "schema hot-reload complete"):
				return true, false
			case strings.Contains(line, "schema reload: rebuild failed"):
				return false, true
			}
		case <-deadline:
			t.Fatal("the server neither accepted nor refused the reload within the " +
				"budget; the notification may not have reached it, in which case " +
				"this test proves nothing either way")
			return false, false
		}
	}
}

// reloadIR returns the checkpoint IR, with or without `partition by`.
func reloadIR(partitioned bool) *dsl.IR {
	e := dsl.Entity{
		Name: "Doc", Namespace: "reloadgate",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true},
			{Name: "tenant", Type: dsl.FieldType{Name: "text"}, NotNull: true},
			{Name: "body", Type: dsl.FieldType{Name: "text"}},
		},
	}
	if partitioned {
		e.PartitionField = "tenant"
	}
	ir := &dsl.IR{Entities: []dsl.Entity{e}}
	// Without proto numbers registration fails with `conflicting fields` long
	// before the gate is reached — see the note in bootFixture.
	codegen.AssignProtoNumbers(nil, ir)
	return ir
}

// seedCheckpoint writes the IR and a content hash derived from a marker.
//
// The hash must CHANGE for the listener to act: it compares the notified hash
// against the snapshot it already holds and returns early when they match. A
// checkpoint written with an unchanged hash produces a notification the server
// correctly ignores, and a test built on that would time out proving nothing.
func seedCheckpoint(t *testing.T, dsn string, ir *dsl.IR, marker string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	raw, err := json.Marshal(ir)
	if err != nil {
		t.Fatalf("marshal ir: %v", err)
	}
	sum := sha256.Sum256([]byte(marker))
	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.ir_checkpoint (id, ir, applied_by, content_hash)
VALUES (1, $1, 'reload-gate-test', $2)
ON CONFLICT (id) DO UPDATE SET ir = EXCLUDED.ir, content_hash = EXCLUDED.content_hash`,
		raw, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
}

// reloadTable creates the physical table, with or without its policy.
func reloadTable(t *testing.T, dsn string, withPolicy bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS atlantis.reloadgate_doc (
  id     bigint PRIMARY KEY,
  tenant text NOT NULL,
  body   text
)`); err != nil {
		t.Fatalf("fixture table: %v", err)
	}
	if !withPolicy {
		return
	}
	if _, err := pool.Exec(ctx, `
ALTER TABLE atlantis.reloadgate_doc ENABLE ROW LEVEL SECURITY;
ALTER TABLE atlantis.reloadgate_doc FORCE ROW LEVEL SECURITY;
CREATE POLICY reloadgate_doc_tenant_isolation ON atlantis.reloadgate_doc AS RESTRICTIVE
  USING (tenant = atlantis.current_partition())
  WITH CHECK (tenant = atlantis.current_partition());
CREATE POLICY reloadgate_doc_default_access ON atlantis.reloadgate_doc AS PERMISSIVE
  USING (true) WITH CHECK (true);`); err != nil {
		t.Fatalf("fixture policy: %v", err)
	}
}

// reloadProbeRole returns a DSN for a role row-level security applies to.
//
// The server must not connect as a superuser or the policies are inert and the
// gate has nothing to detect — the same reason bootProbeRole exists.
func reloadProbeRole(t *testing.T, dsn string) string {
	t.Helper()
	drop := func() {
		pgcatalog.Exec(t, dsn,
			`DROP OWNED BY reloadgate_probe`,
			`DROP ROLE IF EXISTS reloadgate_probe`)
	}
	drop()
	t.Cleanup(drop)

	pgcatalog.Exec(t, dsn,
		`CREATE ROLE reloadgate_probe LOGIN PASSWORD 'probe' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
		`GRANT USAGE ON SCHEMA atlantis TO reloadgate_probe`,
		`GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA atlantis TO reloadgate_probe`,
		`GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA atlantis TO reloadgate_probe`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA atlantis TO reloadgate_probe`,
	)
	return strings.Replace(dsn, "//atlantis:atlantis@", "//reloadgate_probe:probe@", 1)
}

// The reload hook refuses when it could not ask, as boot does.
//
// partitionGate's probe-failure branch has two call sites.
// TestServerRefusesToBootWhenThePolicyProbeCannotRun covers startup; this covers
// the reload hook. Without both, replacing `perr` with nil at either site leaves
// the suite green, and the gate sees no error and no problems and swaps in a
// schema whose isolation nothing verified.
//
// A boot refusal is loud — the process does not come up. A reload that wrongly
// accepts keeps serving with the health surface green, and the dispatcher
// believes a table is tenant-isolated on the word of a check that errored.
//
// The revoke lands after the child is listening. The boot-time probe runs
// against an intact catalogue and passes, so only the reload's probe fails; a
// revoke before boot refuses at startup and never reaches this call site.
//
// Both cases use a table with a valid policy, so they differ only in whether
// pg_policy is readable. An acceptance cannot be explained by the probe failing
// and a refusal cannot be explained by the policy being wrong.
func TestReloadRefusesWhenThePolicyProbeCannotRun(t *testing.T) {
	if os.Getenv(reloadChildEnv) != "" {
		reloadChild()
		return
	}
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the reload gate")
	}

	for _, tc := range []struct {
		name       string
		revoke     bool
		wantAccept bool
		why        string
	}{
		{
			// The control. Without it, a hook that refused every reload would
			// pass the case below and this test would be worthless.
			name: "the probe can run", revoke: false, wantAccept: true,
			why: "the table carries an enforced policy and the catalogue is " +
				"readable, so there is nothing to object to",
		},
		{
			name: "the probe cannot run", revoke: true, wantAccept: false,
			why: "a verification that could not run is not a verification that " +
				"passed, and accepting here swaps in a schema whose isolation " +
				"nothing confirmed while the health surface stays green",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Its own database: the sibling reload test drops atlantis_reloadgate
			// on cleanup, and sharing the name destroys one while the other is
			// still booting a child against it.
			dsn := bootDatabase(t, adminDSN, "atlantis_reloadprobe")
			probeDSN := reloadProbeRole(t, dsn)

			// Boot clean: no `partition by` yet, and the table already carries the
			// policy it will need.
			seedCheckpoint(t, dsn, reloadIR(false), "clean")
			reloadTable(t, dsn, true)

			child, lines := startReloadChild(t, probeDSN)
			defer func() { _ = child.Process.Kill() }()

			if !awaitLine(t, lines, "schema listener: LISTEN active", 60*time.Second) {
				t.Fatal("the child never began listening, so nothing below tests the reload path")
			}

			if tc.revoke {
				revokePolicyCatalogue(t, dsn, "reloadgate_probe")
			}

			// Turn on `partition by` at a running server.
			seedCheckpoint(t, dsn, reloadIR(true), "partitioned")

			accepted, refused := awaitVerdict(t, lines, 60*time.Second)
			t.Logf("reload verdict: accepted=%v refused=%v", accepted, refused)
			if !accepted && !refused {
				t.Fatal("no verdict observed; the assertions below would pass for a " +
					"server that never reloaded at all")
			}
			switch {
			case tc.wantAccept && !accepted:
				t.Errorf("the reload was refused and should not have been: %s", tc.why)
			case !tc.wantAccept && accepted:
				t.Errorf("the reload was accepted and should not have been: %s", tc.why)
			}
		})
	}
}
