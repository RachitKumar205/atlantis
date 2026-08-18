package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
	"github.com/rachitkumar205/atlantis/migrations"
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
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, dbName)

	// The console refuses to start on a role that reads through row-level
	// security (see consoleRoleError), and the test database's default role is
	// a superuser. So the harness connects as a role the policies will actually
	// apply to — which is also the posture a deployment runs in, making this
	// harness a closer model of production than it was.
	//
	// CREATE on the database because console.New runs its own migrations, which
	// create the console schema and everything in it. The role therefore OWNS
	// what it creates, which is what FORCE ROW LEVEL SECURITY binds against
	// once step 3 adds the policies.
	consoleDSN := isolatedRoleDSN(t, adminDSN, dsn, dbName, "console_probe")

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Infra only. The console's own schema is applied by console.New from the
	// tree embedded in the binary, which is the path under test.
	if err := migrate.RunFS(dsn, migrations.Infra, "infra",
		migrate.InfraHistoryTable, quiet); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The real admin service, on a real listener, over real mTLS.
	//
	// The listener demands and verifies a client certificate, exactly as
	// cmd/server's does. That is not extra rigour for its own sake: the console
	// has no insecure transport any more, so a plaintext test listener would be
	// a channel the console cannot dial at all.
	pki := testpki.New(t, t.TempDir())
	consoleCert, consoleKey := pki.ClientCert(t, "atlantis-console")

	adminSv := admin.New(pool, admin.Config{AllowApplyMutation: true})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLS(t))))
	admin.RegisterGenerated(grpcSrv, adminSv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	srv, err := New(Config{
		PGURL:               consoleDSN,
		ATLEndpoint:         lis.Addr().String(),
		SessionSecret:       strings.Repeat("k", 32),
		SandboxPerUserLimit: 1,
		SandboxTTL:          time.Minute,
		ATLTLSCert:          consoleCert,
		ATLTLSKey:           consoleKey,
		ATLTLSCA:            pki.CAFile,
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

// isolatedRoleDSN creates a role the console can legitimately run as, and
// returns a DSN for it against the same database.
//
// NOSUPERUSER NOBYPASSRLS is the whole point: the console refuses to start on
// anything else, because a role that reads through row-level security makes the
// organisation boundary inert. Ten other tests in this repo create a role this
// way for the same reason; the grants differ per test, which is why each does
// its own rather than sharing one helper.
//
// CREATE on the database because console.New runs its own migrations. The role
// creates the console schema and therefore owns it, which is the state FORCE
// ROW LEVEL SECURITY needs to bind against.
func isolatedRoleDSN(t *testing.T, adminDSN, dbDSN, dbName, role string) string {
	t.Helper()

	const password = "probe"
	pgcatalog.Do(t, dbDSN, func(conn *pgx.Conn) error {
		ctx := context.Background()
		// Dropped first: a previous run in the same cluster may have left it,
		// and CREATE ROLE is not idempotent.
		_, _ = conn.Exec(ctx, `DROP OWNED BY `+pgx.Identifier{role}.Sanitize())
		_, _ = conn.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
		for _, sql := range []string{
			fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`,
				pgx.Identifier{role}.Sanitize(), password),
			fmt.Sprintf(`GRANT CREATE, CONNECT ON DATABASE %s TO %s`,
				pgx.Identifier{dbName}.Sanitize(), pgx.Identifier{role}.Sanitize()),
			// The infra tree is already applied by the admin role above, and the
			// console reads atlantis.* through the admin gRPC service rather
			// than directly — but the migration runner's history table lives in
			// public, so it needs to write there.
			fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`,
				pgx.Identifier{role}.Sanitize()),
		} {
			if _, err := conn.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		return nil
	})
	t.Cleanup(func() {
		pgcatalog.Exec(t, dbDSN,
			`DROP OWNED BY `+pgx.Identifier{role}.Sanitize(),
			`DROP ROLE IF EXISTS `+pgx.Identifier{role}.Sanitize())
	})

	u, err := url.Parse(dbDSN)
	if err != nil {
		t.Fatalf("parse %q: %v", dbDSN, err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}
