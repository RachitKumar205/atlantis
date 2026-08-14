package console

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// The console's first HTTP test harness.
//
// # Why this exists, and why it is this shape
//
// Until now internal/console had one test, of a bcrypt hash. Every route was
// covered by nothing — including the approve route, which is the control that
// lets production DDL run, and which is wrapped in four middlewares whose
// order matters.
//
// The obstacle is that a *Server holds an *adminClient, a concrete type that
// opens a gRPC connection. There is no *Server without something answering on
// the other end. Two ways past that were available:
//
//   - Make the field an interface and pass a fake. Rejected: it changes shipped
//     code to suit a test, and it would test the console against a fake whose
//     behaviour somebody has to keep in step with the real server by hand.
//   - Run the real admin service in-process. Chosen. dialAdmin already falls
//     back to insecure credentials when no TLS cert is configured, so a plain
//     listener is enough, and New() is used exactly as production uses it.
//
// The second is slower to set up and tests the thing that ships. It also means
// the assertions below run against real plan rows created by a real apply,
// rather than fixtures shaped like what the code hopes it will be handed.
//
// # What is deliberately absent
//
// No capability interceptor. cmd/server installs it; RegisterGenerated does
// not. That is correct here: these tests are about the console's HTTP gates,
// and admin-side authorization has its own tests in internal/server/authz.

// consoleFixture is a console server wired to a real admin service, both
// backed by one private database.
type consoleFixture struct {
	srv     *Server
	adminSv *admin.Service
	pool    *pgxpool.Pool
	dsn     string
}

func newConsoleFixture(t *testing.T) *consoleFixture {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the console's HTTP routes")
	}

	// Confine this process's temp directory before New() runs.
	//
	// New() calls sweepEmbeddedTempdirs, which os.RemoveAll's every
	// atlantis-sandbox-{data,runtime}-* under os.TempDir(). `go test` runs
	// packages concurrently, so without this the console's tests delete the
	// working directories of internal/runtime/sandbox and its two sibling
	// packages while they are extracting a Postgres archive into them. The
	// first version of this harness did exactly that: three packages failed
	// with "rename ...: no such file or directory" and the cause was two
	// directories away from anything the console changed.
	//
	// t.Setenv is process-wide and forbidden alongside t.Parallel, which is
	// what makes it safe here — console tests are sequential, and other
	// packages run in their own processes.
	t.Setenv("TMPDIR", t.TempDir())

	const dbName = "atlantis_console_http"
	drop := func() {
		pgcatalog.Exec(t, adminDSN,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '`+dbName+`'`,
			`DROP DATABASE IF EXISTS `+dbName)
	}
	drop()
	t.Cleanup(drop)
	pgcatalog.Exec(t, adminDSN, `CREATE DATABASE `+dbName)

	i := strings.LastIndex(adminDSN, "/atlantis?")
	if i < 0 {
		t.Fatalf("cannot derive a DSN for %s from %q", dbName, adminDSN)
	}
	dsn := adminDSN[:i] + "/" + dbName + adminDSN[i+len("/atlantis"):]

	pgcatalog.Exec(t, dsn,
		`CREATE EXTENSION IF NOT EXISTS citext`,
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.Run(dsn, "../../migrations", quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The real admin service, on a real listener.
	adminSv := admin.New(pool, admin.Config{AllowApplyMutation: true})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	admin.RegisterGenerated(grpcSrv, adminSv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	srv, err := New(Config{
		PGURL:               dsn,
		ATLEndpoint:         lis.Addr().String(),
		SessionSecret:       strings.Repeat("k", 32),
		SandboxPerUserLimit: 1,
		SandboxTTL:          time.Minute,
	}, nil, quiet)
	if err != nil {
		t.Fatalf("console New: %v", err)
	}
	t.Cleanup(srv.Close)

	return &consoleFixture{srv: srv, adminSv: adminSv, pool: pool, dsn: dsn}
}

// signIn creates a user with the given role and returns a session token.
func (f *consoleFixture) signIn(t *testing.T, email, role string) string {
	t.Helper()
	ctx := context.Background()
	u, err := f.srv.db.createUser(ctx, email, "correct-horse-battery-staple", role, "Test", "User")
	if err != nil {
		t.Fatalf("create %s: %v", email, err)
	}
	tok, err := f.srv.db.createSession(ctx, u.ID)
	if err != nil {
		t.Fatalf("create session for %s: %v", email, err)
	}
	return tok
}

// elevate puts a session into sudo mode, as /api/auth/sudo does after the
// operator re-types their password.
func (f *consoleFixture) elevate(t *testing.T, token string) {
	t.Helper()
	if err := f.srv.db.grantSudo(context.Background(), token); err != nil {
		t.Fatalf("grant sudo: %v", err)
	}
}

// request builds a same-origin POST carrying the session cookie.
func (f *consoleFixture) request(t *testing.T, method, path, body, token string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, "http://console.test"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "console.test"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	}
	return r
}

// pendingPlan drives a real destructive apply so a real request is waiting.
//
// Built through the admin service rather than by inserting a row, because a
// hand-written row is a guess about what the gate produces — and the console
// reads columns the gate fills in, not columns a test author remembered.
func (f *consoleFixture) pendingPlan(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	const withNote = `
entity Ledger in conhttp {
  id   bigint primary
  note text
}
`
	const withoutNote = `
entity Ledger in conhttp {
  id bigint primary
}
`
	apply := func(src string) (*adminpb.PlanSchemaResponse, error) {
		files := []*adminpb.SubmittedFile{{Path: "ledger.atl", Content: []byte(src)}}
		plan, err := f.adminSv.PlanSchema(ctx, &adminpb.PlanSchemaRequest{Caller: "conhttp", Files: files})
		if err != nil {
			t.Fatalf("PlanSchema: %v", err)
		}
		_, err = f.adminSv.ApplyMigration(ctx, &adminpb.ApplyMigrationRequest{
			Caller: "conhttp", PlanId: plan.GetPlanId(), Files: files,
			CheckpointHash: plan.GetCheckpointHash(),
		})
		return plan, err
	}

	if _, err := apply(withNote); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
	plan, err := apply(withoutNote)
	if err == nil {
		t.Fatal("the destructive apply was not refused, so no plan is waiting")
	}
	return plan.GetPlanId()
}

func (f *consoleFixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM console.audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}
