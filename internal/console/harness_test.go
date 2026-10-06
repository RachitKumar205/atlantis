package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
	"github.com/rachitkumar205/atlantis/migrations"
)

// The console's first HTTP test harness.
//
// Without it, routes are covered by nothing — including the approve route, the
// control that lets production DDL run, wrapped in four middlewares whose order
// matters.
//
// The obstacle is that the console talks to atlantis over a concrete gRPC
// channel, one per organisation from orgClients, so there is no working *Server
// without something answering on the other end. It runs the real admin service
// in-process rather than putting an interface in front of it and passing a
// fake: New() is used as production uses it, and the assertions below run
// against real plan rows created by a real apply.
//
// The listener speaks mTLS because the console has no insecure transport at all
// — dialOrg has no plaintext branch — so a plain listener is a channel it
// cannot dial.
//
// No capability interceptor: cmd/server installs it and RegisterGenerated does
// not. These tests are about the console's HTTP gates, and admin-side
// authorization has its own tests in internal/server/authz.

// consoleFixture is a console server wired to a real admin service, both
// backed by one private database.
type consoleFixture struct {
	srv     *Server
	adminSv *admin.Service
	pool    *pgxpool.Pool
	dsn     string

	// consoleDSN is the isolated role the console itself runs as, as opposed
	// to dsn, which is the administrative one the fixture inspects with. A
	// test that goes through an exported entry point should use this, so it
	// exercises the grants a deployment has rather than a superuser's.
	consoleDSN string

	// keyset is what the console was started with. Registration has to happen
	// under the same one, or the row is written and never opens.
	keyset string

	// A real Cloud issuer, on a real JWKS endpoint. Not a stub: the console
	// has no development bypass for identity, so a fixture that faked one
	// would be exercising a path production never takes.
	iss      *issuer.Issuer
	audience string

	// atl is the default organisation's stack, kept so a test about the
	// boundary can reach its CA and its certificate, which are the two halves
	// a mismatched pair is built from.
	atl *atlStack

	// signer is the fake certificate signer, or nil when the fixture was built
	// without enrolment.
	signer *fakeSigner
}

func newConsoleFixture(t *testing.T) *consoleFixture {
	t.Helper()
	return newFixture(t, false)
}

// newEnrolmentFixture is the same console with certificate enrolment configured
// and a fake signer behind it.
//
// Separate because enrolment is off by default and most of this package is
// about something else — and because a fixture that always stood up a signer
// would make every unrelated test depend on it.
func newEnrolmentFixture(t *testing.T) *consoleFixture {
	t.Helper()
	return newFixture(t, true)
}

// newFixture applies opts to the console's Config before New.
func newFixture(t *testing.T, enrolment bool, opts ...func(*Config)) *consoleFixture {
	t.Helper()
	adminDSN := requireTestPG(t)

	// Confine this process's temp directory before New() runs.
	//
	// New() calls sweepEmbeddedTempdirs, which os.RemoveAll's every
	// atlantis-sandbox-{data,runtime}-* under os.TempDir(). `go test` runs
	// packages concurrently, so without this the console's tests delete the
	// working directories of internal/runtime/sandbox and its two sibling
	// packages while they are extracting a Postgres archive into them, which
	// fails those packages with "rename ...: no such file or directory" two
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

	// The default organisation's atlantis, behind its own CA. Tests about the
	// boundary stand up a second one the same way.
	stack := newATLStack(t, dsn)

	// Cloud, as far as this console is concerned: a signing key, a JWKS
	// endpoint, and an issuer name the console is configured to trust.
	signingKey, err := issuer.GenerateKey()
	if err != nil {
		t.Fatalf("generate cloud signing key: %v", err)
	}
	iss, err := issuer.New("https://cloud.test", signingKey)
	if err != nil {
		t.Fatalf("issuer.New: %v", err)
	}
	jwks := httptest.NewServer(iss.Handler())
	t.Cleanup(jwks.Close)

	const audience = "https://console.test"

	keyset := testKeyset(t)
	cfg := Config{
		PGURL:               consoleDSN,
		SessionSecret:       strings.Repeat("k", 32),
		DataKeyset:          keyset,
		SandboxPerUserLimit: 1,
		SandboxTTL:          time.Minute,
		CloudIssuer:         iss.Name(),
		CloudAudience:       audience,
		CloudJWKSURL:        jwks.URL + issuer.JWKSPath,
	}

	var signer *fakeSigner
	if enrolment {
		// The signer issues from the ORGANISATION's authority — stack.pki —
		// because that is what the organisation's atlantis trusts, and
		// issueForCaller verifies the returned leaf against it before recording
		// anything.
		//
		// It accepts callers by a SECOND authority, signerPKI, which is the
		// arrangement production requires: every leaf the signer issues carries
		// ExtKeyUsage: ClientAuth, so a signer trusting its own issuing CA would
		// accept everything it had ever produced as a credential to itself.
		signer = newFakeSigner(t, stack.pki)
		cfg.SignerAddr = signer.URL
		cfg.SignerCert, cfg.SignerKey = signer.clientCert, signer.clientKey
		cfg.SignerCA = signer.pki.CAFile

		// The enrolment listener's own certificate chains to the ORGANISATION's
		// authority, which is what deploy/init-certs.sh does and for the same
		// reason: the machines that dial it are callers, and they already hold
		// that CA. First contact is the one moment a machine has no certificate
		// of its own, so giving it one fewer thing to obtain matters there.
		//
		// Its client CA is that authority too, so a machine renewing presents a
		// certificate the listener can verify.
		cfg.EnrollListen = "127.0.0.1:0"
		cfg.EnrollTLSCert, cfg.EnrollTLSKey = stack.pki.CertFile, stack.pki.KeyFile
		// No client CA: the listener asks for a certificate and handleRenew
		// verifies it against the organisation's own authority, which is the
		// only thing that can work once each organisation has one.

		// The address a machine is told to come back to. The listener binds
		// :0 here, so this is not derived from it — which is the same reason
		// the setting exists at all: a bind address says nothing about how
		// anything outside reaches you.
		cfg.EnrollPublicURL = "https://console.test:3443"
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	srv, err := New(cfg, nil, quiet)
	if err != nil {
		t.Fatalf("console New: %v", err)
	}
	t.Cleanup(srv.Close)

	f := &consoleFixture{
		srv: srv, adminSv: stack.svc, pool: stack.pool, dsn: dsn,
		consoleDSN: consoleDSN, keyset: keyset,
		iss: iss, audience: audience, atl: stack, signer: signer,
	}

	// The console has no process-wide endpoint or certificate any more, so an
	// organisation is unreachable until it is registered. Registering the
	// default one here keeps every existing test working; tests about the
	// boundary register a second organisation against a second CA.
	f.registerOrg(t, defaultOrg, stack)
	return f
}

// requireTestPG returns the administrative DSN, or skips.
//
// CI asserts that no test in this repo skips for this reason
// (.github/workflows/ci.yml), so the skip is a local-development convenience
// rather than a way for these tests to be quietly absent.
func requireTestPG(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the console against a real database")
	}
	return dsn
}

// atlStack is one organisation's atlantis: its own database, its own
// certificate authority, and a listener that demands a certificate from it.
//
// One per organisation, with the CA per stack rather than shared, which is what
// the per-organisation client pool rests on.
// With one CA behind two servers, credentials issued for either organisation
// chain at both, every cross-organisation dial succeeds, and a test asserting
// "A's client returned A's data" passes for the weaker reason that the pool
// happened to hand back the right channel. With two, the wrong pairing is
// refused inside the handshake, which is the claim the design actually makes.
type atlStack struct {
	pki      *testpki.PKI
	certFile string
	keyFile  string
	addr     string
	svc      *admin.Service
	pool     *pgxpool.Pool
	dsn      string

	// healthAddr is the mTLS health listener, carrying /status as cmd/server
	// does. The fleet sweep reads it for every organisation on every pass.
	healthAddr string

	// status is what /status answers, so a test can make it fail or stall
	// without taking the gRPC service down with it. Guarded because the sweep
	// reads it from its own goroutines.
	statusMu sync.Mutex
	status   func() (int, string)

	// stopHealth takes the health listener down, which is what a tenant whose
	// pod has gone looks like. Editing the registry row instead would not
	// reach a sweep for orgClientRefresh, since the cached entry is returned
	// without re-reading the row.
	stopHealth func()

	// stopGRPC takes the admin service down and leaves /status answering:
	// a tenant whose health listener is up and whose admin plane is not.
	stopGRPC func()
}

// setStatus replaces what the health listener answers.
func (s *atlStack) setStatus(fn func() (int, string)) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status = fn
}

// newATLStack runs the real admin service on a real mTLS listener over dsn.
//
// The caller creates and migrates the database, because the fixture's first
// stack shares one with the console's own schema and a second one does not.
func newATLStack(t *testing.T, dsn string) *atlStack {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	pki := testpki.New(t, t.TempDir())
	certFile, keyFile := pki.ClientCert(t, "atlantis-console")

	svc := admin.New(pool, admin.Config{AllowApplyMutation: true})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.ServerTLS(t))))
	admin.RegisterGenerated(grpcSrv, svc)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	stack := &atlStack{
		pki: pki, certFile: certFile, keyFile: keyFile,
		addr: lis.Addr().String(), svc: svc, pool: pool, dsn: dsn,
		stopGRPC: grpcSrv.Stop,
	}

	// The health listener, terminating its own TLS as cmd/server's does. Stood
	// up for every stack: the fleet sweep reads /status per organisation, and a
	// dead address here would make every sweep in every test report the
	// organisation unreachable.
	healthLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen health: %v", err)
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		stack.statusMu.Lock()
		fn := stack.status
		stack.statusMu.Unlock()
		if fn != nil {
			code, body := fn()
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
			return
		}
		// As cmd/server's statusHandler: 0 for no versions, and no field when
		// the read fails.
		var version *int64
		err := pool.QueryRow(context.Background(),
			`SELECT MAX(version) FROM atlantis.schema_versions`).Scan(&version)
		out := map[string]any{
			"started_at": time.Now().UTC().Format(time.RFC3339),
			"version":    "test",
		}
		switch {
		case err != nil:
		case version == nil:
			out["schema_version"] = 0
		default:
			out["schema_version"] = *version
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	healthSrv := &http.Server{
		Handler:           healthMux,
		TLSConfig:         pki.ServerTLS(t),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = healthSrv.ServeTLS(healthLis, "", "") }()
	t.Cleanup(func() { _ = healthSrv.Close() })
	stack.healthAddr = healthLis.Addr().String()
	stack.stopHealth = func() { _ = healthSrv.Close() }

	return stack
}

// credentials is what `cloud org register` would be handed for this stack.
//
// Reads the PEM off disk because testpki writes files, while the console keeps
// the material in its registry row — the console no longer loads certificates
// from paths at all.
func (s *atlStack) credentials(t *testing.T, org string) orgCredentials {
	t.Helper()
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	return orgCredentials{
		Org:        org,
		Endpoint:   s.addr,
		HealthAddr: s.healthAddr,
		CAPEM:      read(s.pki.CAFile),
		CertPEM:    read(s.certFile),
		KeyPEM:     []byte(read(s.keyFile)),
	}
}

// registerOrg points an organisation at a stack, as `cloud org register` does
// in a deployment.
func (f *consoleFixture) registerOrg(t *testing.T, org string, s *atlStack) {
	t.Helper()
	if err := f.srv.db.registerOrg(context.Background(), s.credentials(t, org)); err != nil {
		t.Fatalf("register %s: %v", org, err)
	}
}

// defaultOrg is the organisation single-org tests run in. Tests about the
// organisation boundary use assertionForOrg with two of them.
const defaultOrg = "acme"

// subjectFor is the Cloud subject the fixture mints for a user.
//
// Defined once so a test asserting on the actor does not hardcode the format —
// two did, and both broke when the organisation was folded in.
func subjectFor(org, email string) string { return "usr_" + org + "_" + email }

// assertion mints a signed assertion for a user in the default organisation.
func (f *consoleFixture) assertion(t *testing.T, email, role string) string {
	t.Helper()
	return f.assertionForOrg(t, defaultOrg, email, role)
}

// assertionForOrg mints a signed assertion, as Cloud would.
//
// The organisation is a parameter because it is the boundary under test.
// Hardcoded, every test in the package runs as one organisation, no test can
// observe the boundary, and a policy that isolates nothing passes the suite.
func (f *consoleFixture) assertionForOrg(t *testing.T, org, email, role string) string {
	t.Helper()
	return f.mint(t, org, email, role, false, nil)
}

// assertionWithOrgs mints one that also names the memberships, as Cloud's
// /authorize does for a console's organisation switcher.
//
// A separate helper rather than another parameter on the one above, because
// almost every test in this package is about something else and an empty list
// is what a session with nothing to switch to actually carries.
func (f *consoleFixture) assertionWithOrgs(t *testing.T, org, email, role string, orgs []string) string {
	t.Helper()
	return f.mint(t, org, email, role, false, orgs)
}

// stepUpAssertion mints what Cloud's POST /api/orgs/{org}/step-up produces.
//
// The difference from an ordinary assertion is one claim, and it is the whole
// of the step-up gate: an ordinary one says somebody holds a Cloud session,
// which can be twelve hours old, and this one says somebody presented a second
// factor. Sudo requires the second.
func (f *consoleFixture) stepUpAssertion(t *testing.T, email, role string) string {
	t.Helper()
	return f.mint(t, defaultOrg, email, role, true, nil)
}

func (f *consoleFixture) mint(t *testing.T, org, email, role string, stepUp bool, orgs []string) string {
	t.Helper()
	// Subject is scoped by organisation as well as email. Cloud subjects are
	// globally unique, and two organisations having genuinely different people
	// at the same address is the case a shared subject would quietly merge.
	return f.mintAs(t, subjectFor(org, email), org, email, role, stepUp, orgs)
}

// mintAs mints for a subject the caller names.
//
// One case needs it: the organisation switch. That is one person moving between
// organisations, and a Cloud subject is the user's id — so the subject is
// exactly what does not change across it. Deriving it from the organisation, as
// every other test here wants, would make the switched session look like a
// different person, and a test about the organisation half of an ownership key
// would then pass on the subject half instead.
func (f *consoleFixture) mintAs(t *testing.T, subject, org, email, role string, stepUp bool, orgs []string) string {
	t.Helper()
	tok, err := f.iss.Mint(issuer.Grant{
		Subject:  subject,
		Org:      org,
		Role:     identity.Role(role),
		Email:    email,
		Name:     "Test User",
		Audience: f.audience,
		StepUp:   stepUp,
		Orgs:     orgs,
	})
	if err != nil {
		t.Fatalf("mint assertion for %s in %s: %v", email, org, err)
	}
	return tok
}

// signIn returns a session token for a user with the given role.
//
// It goes through the real exchange endpoint rather than writing a session
// row. Every test that calls this then depends on the actual sign-in path, so
// a change that breaks it cannot pass by leaving a shortcut intact.
func (f *consoleFixture) signIn(t *testing.T, email, role string) string {
	t.Helper()
	return f.signInToOrg(t, defaultOrg, email, role)
}

// signInToOrg returns a session token for a user in a named organisation.
func (f *consoleFixture) signInToOrg(t *testing.T, org, email, role string) string {
	t.Helper()
	return f.exchange(t, f.assertionForOrg(t, org, email, role), "")
}

// exchange spends an assertion at the real endpoint and returns the session
// cookie it sets.
//
// carry is the session cookie the browser already holds: empty for a first
// sign-in, and the live session for an organisation switch. The second case is
// the one worth having a parameter for — it is where the console has to replace
// a session rather than open a second one beside it.
func (f *consoleFixture) exchange(t *testing.T, assertion, carry string) string {
	t.Helper()

	body := fmt.Sprintf(`{"assertion":%q}`, assertion)
	req := f.request(t, http.MethodPost, "/api/auth/exchange", body, carry)
	rec := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("exchange: status %d, body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("exchange set no session cookie")
	return ""
}

// elevate puts a session into sudo mode, as /api/auth/sudo does after the
// operator presents a fresh assertion.
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
// NOSUPERUSER NOBYPASSRLS, because the console refuses to start on anything
// else: a role that reads through row-level security makes the organisation
// boundary inert.
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

// enrolOrg puts an organisation in the registry and nothing else.
//
// console.enroll_tokens and console.schema_imports carry a foreign key to
// console.orgs since migration 0011, so a test writing either for a second
// organisation needs a registry row for it. registerOrg is the full path and
// wants a stack to point at; a boundary test needs neither.
func (f *consoleFixture) enrolOrg(t *testing.T, org string) {
	t.Helper()
	if _, err := f.srv.db.pool.Exec(context.Background(),
		`INSERT INTO console.orgs (org) VALUES ($1) ON CONFLICT (org) DO NOTHING`,
		org); err != nil {
		t.Fatalf("register %s: %v", org, err)
	}
}
