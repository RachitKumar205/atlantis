package console

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/connectivity"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/server/admin"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
	"github.com/rachitkumar205/atlantis/migrations"
)

// The organisation boundary on the wire.
//
// Everything else in this package tests one organisation against one atlantis,
// which is the configuration in which the whole per-organisation client pool
// could be deleted and replaced by a single shared channel without a single
// failing test. These tests stand up two of everything — two databases, two
// certificate authorities, two servers, two schemas — because that is the only
// arrangement in which "the console reached the right one" is a claim that can
// come out false.
//
// The schemas differ: each stack gets an entity the other does not have. Where
// both servers answer identically, a correct lookup and a shared client return
// the same bytes and the assertion passes either way. Asserting that B's entity
// is absent from A's answer is the half that does the work.

// otherOrg is the second organisation. `acme` (defaultOrg) is the first.
const otherOrg = "globex"

// twoOrgs is a console serving two organisations, each with its own atlantis.
type twoOrgs struct {
	*consoleFixture
	other *atlStack
}

// newTwoOrgFixture adds a second organisation, on its own database, behind its
// own certificate authority.
//
// testpki.New is called a second time rather than reusing the fixture's PKI,
// and that is required rather than stylistic in two ways. A second CA is the
// property under test. And PKI.ClientCert derives its filenames from the CN, so
// two `atlantis-console` certificates issued from one PKI overwrite each
// other's files — the second call would silently hand back the first's key.
func newTwoOrgFixture(t *testing.T) *twoOrgs {
	t.Helper()

	f := newConsoleFixture(t)

	adminDSN := requireTestPG(t)
	dsn := pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_console_org_b")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.RunFS(dsn, migrations.Infra, "infra",
		migrate.InfraHistoryTable, quiet); err != nil {
		t.Fatalf("migrate the second organisation's database: %v", err)
	}

	other := newATLStack(t, dsn)
	f.registerOrg(t, otherOrg, other)

	// One entity each, named differently, so a mixed-up client returns
	// visibly wrong data rather than an empty result — which is the failure a
	// weaker fixture would report as success.
	applyEntity(t, f.adminSv, "Ledger")
	applyEntity(t, other.svc, "Vault")

	return &twoOrgs{consoleFixture: f, other: other}
}

// applyEntity puts one entity into a stack's schema, through a real apply.
func applyEntity(t *testing.T, svc *admin.Service, name string) {
	t.Helper()
	ctx := context.Background()
	src := "\nentity " + name + " in orgb {\n  id bigint primary\n}\n"
	files := []*adminpb.SubmittedFile{{Path: strings.ToLower(name) + ".atl", Content: []byte(src)}}

	plan, err := svc.PlanSchema(ctx, &adminpb.PlanSchemaRequest{Caller: "orgb", Files: files})
	if err != nil {
		t.Fatalf("PlanSchema %s: %v", name, err)
	}
	if _, err := svc.ApplyMigration(ctx, &adminpb.ApplyMigrationRequest{
		Caller: "orgb", PlanId: plan.GetPlanId(), Files: files,
		CheckpointHash: plan.GetCheckpointHash(),
	}); err != nil {
		t.Fatalf("ApplyMigration %s: %v", name, err)
	}
}

// schemaFor signs in to an organisation and returns the .atl source behind
// GET /api/schema, through the real route and the real session.
//
// Decoded rather than matched against the raw body: proto JSON carries `bytes`
// as base64, so the entity names are not in the response text. A test that
// searched the body for "Ledger" would find nothing and fail whether or not
// the routing was right — which is how this one first behaved.
func (t2 *twoOrgs) schemaFor(t *testing.T, org string) string {
	t.Helper()
	token := t2.signInToOrg(t, org, "user@"+org+".test", "admin")
	req := t2.request(t, http.MethodGet, "/api/schema", "", token)
	rec := httptest.NewRecorder()
	t2.srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/schema as %s: status %d, body %s", org, rec.Code, rec.Body.String())
	}

	var resp struct {
		Files []struct {
			Path string `json:"path"`
			// Text: the BFF decodes the proto `bytes` field. See proxyProtoText.
			Content string `json:"content"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode the schema for %s: %v\n%s", org, err, rec.Body.String())
	}
	if len(resp.Files) == 0 {
		t.Fatalf("%s was served an empty schema, so the assertions below cannot "+
			"tell a correct lookup from a broken one: %s", org, rec.Body.String())
	}

	var sb strings.Builder
	for _, f := range resp.Files {
		sb.WriteString(f.Path)
		sb.WriteString("\n")
		sb.WriteString(f.Content)
	}
	return sb.String()
}

// Each organisation reaches its own atlantis, and only its own.
func TestEachOrganisationReachesItsOwnAtlantis(t *testing.T) {
	f := newTwoOrgFixture(t)

	acme := f.schemaFor(t, defaultOrg)
	globex := f.schemaFor(t, otherOrg)

	if !strings.Contains(acme, "Ledger") {
		t.Errorf("%s did not get its own schema back: %s", defaultOrg, acme)
	}
	if !strings.Contains(globex, "Vault") {
		t.Errorf("%s did not get its own schema back: %s", otherOrg, globex)
	}

	// The half that fails when one shared client serves both. Without these
	// two, a pool that returned the same channel for every organisation would
	// pass the two assertions above for whichever organisation it happened to
	// be pointed at, and the other's would be an empty schema rather than a
	// wrong one.
	if strings.Contains(acme, "Vault") {
		t.Errorf("%s was served %s's schema: %s", defaultOrg, otherOrg, acme)
	}
	if strings.Contains(globex, "Ledger") {
		t.Errorf("%s was served %s's schema: %s", otherOrg, defaultOrg, globex)
	}
}

// A certificate issued for one organisation is refused by another's server,
// inside the TLS handshake.
//
// The layer is the assertion. Every other control in atlantis passes a
// cross-organisation console certificate: the CN is `atlantis-console` on both,
// migration 0019 seeds that caller with CAPABILITY_OPERATOR in every install,
// and ATL_CERT_BINDING_EXEMPT_CALLERS names it by default. So an error alone
// proves nothing — a test satisfied by any failure would also pass if the
// refusal came from authorization, which would mean the handshake had accepted
// credentials it should not have.
func TestACrossOrganisationCertificateIsRefusedAtTheHandshake(t *testing.T) {
	f := newTwoOrgFixture(t)

	// The other organisation's address, this organisation's credentials. Only
	// a mismatched pair can arise this way; the pool always takes both halves
	// from one row, which is what makes it impossible by construction.
	mixed := f.atl.credentials(t, otherOrg)
	mixed.Endpoint = f.other.addr

	if err := f.srv.db.registerOrg(context.Background(), mixed); err != nil {
		t.Fatalf("register the mismatched pair: %v", err)
	}
	// Whatever the pool cached for this organisation was built from the row it
	// replaced, and would answer from the correct server.
	f.srv.orgs.evict(otherOrg)

	atl, err := f.srv.atlFor(context.Background(), otherOrg)
	if err != nil {
		t.Fatalf("build a client for the mismatched pair: %v", err)
	}
	// grpc.NewClient is lazy, so the handshake happens here rather than above.
	_, err = atl.GetMergedSchema(context.Background(), &adminpb.GetMergedSchemaRequest{})
	if err == nil {
		t.Fatal("one organisation's certificate was accepted by another's atlantis. " +
			"Nothing further along refuses it: the CN is the same, the caller is " +
			"allowlisted in every install, and it holds CAPABILITY_OPERATOR")
	}

	msg := strings.ToLower(err.Error())
	handshake := strings.Contains(msg, "certificate") ||
		strings.Contains(msg, "tls") ||
		strings.Contains(msg, "handshake")
	if !handshake {
		t.Errorf("the call failed, but not in the handshake — so the credentials "+
			"were accepted and something later refused them, which is the layer "+
			"this design exists to move off: %v", err)
	}
}

// An organisation nobody registered is refused, by name, and nothing is
// dialled on its behalf.
func TestAnUnregisteredOrganisationIsRefused(t *testing.T) {
	f := newConsoleFixture(t)

	const unknown = "initech"
	token := f.signInToOrg(t, unknown, "user@initech.test", "admin")

	req := f.request(t, http.MethodGet, "/api/schema", "", token)
	rec := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(rec, req)

	// 503 rather than 404: the organisation is real, somebody signed in to it,
	// and what is missing is a stack. That is an operator's problem.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unregistered organisation got status %d, want 503. A fallback "+
			"endpoint would render this page from somebody else's atlantis: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), unknown) {
		t.Errorf("the error does not name the organisation, so an operator cannot "+
			"tell which one to register: %s", rec.Body.String())
	}
}

// The private key is not in the row as PEM.
func TestThePrivateKeyIsNotStoredInPlaintext(t *testing.T) {
	f := newConsoleFixture(t)

	// As the owning role, which is the strongest read anyone gets short of the
	// console's own key: a leaked backup or a replica is exactly this.
	var ct []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT client_key_ct FROM console.orgs WHERE org = $1`, defaultOrg).Scan(&ct); err != nil {
		t.Fatalf("read the sealed key: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("the column is empty, so this test would pass against a console " +
			"that stored no key at all")
	}
	if strings.Contains(string(ct), "PRIVATE KEY") {
		t.Fatal("the private key is in the database as PEM")
	}

	// And the counterpart: it is the right key, sealed. Without this the test
	// above passes against a column full of unrelated bytes.
	creds, err := f.srv.db.orgCredentials(context.Background(), defaultOrg)
	if err != nil {
		t.Fatalf("read the credentials back: %v", err)
	}
	if !strings.Contains(string(creds.KeyPEM), "PRIVATE KEY") {
		t.Errorf("the decrypted key is not PEM, so the round trip does not " +
			"reproduce what was stored")
	}
}

// A sealed key cannot be moved onto another organisation's row.
//
// This is the associated-data binding, and it is what converts an UPDATE on
// console.orgs from a credential theft into nothing. Without it, somebody who
// can write that table lifts one organisation's key onto another's row and
// every layer downstream is satisfied — the credential is genuine, it is simply
// being presented by the wrong tenant.
func TestCiphertextCannotBeMovedBetweenOrganisations(t *testing.T) {
	f := newTwoOrgFixture(t)
	ctx := context.Background()

	// Both rows open on their own terms first, so a failure below is the move
	// and not a broken fixture.
	for _, org := range []string{defaultOrg, otherOrg} {
		if _, err := f.srv.db.orgCredentials(ctx, org); err != nil {
			t.Fatalf("%s does not decrypt before the move: %v", org, err)
		}
	}

	if _, err := f.pool.Exec(ctx, `
		UPDATE console.orgs SET client_key_ct = (
			SELECT client_key_ct FROM console.orgs WHERE org = $1
		) WHERE org = $2
	`, defaultOrg, otherOrg); err != nil {
		t.Fatalf("move the ciphertext: %v", err)
	}

	_, err := f.srv.db.orgCredentials(ctx, otherOrg)
	if err == nil {
		t.Fatal("one organisation's sealed key opened on another's row. Anyone " +
			"who can UPDATE console.orgs now holds a working credential for " +
			"every organisation in it")
	}
	if !strings.Contains(err.Error(), otherOrg) {
		t.Errorf("the error does not name the organisation that failed to open: %v", err)
	}
}

// A rotated certificate comes into use without a restart, and the channel it
// replaces is closed.
func TestARotatedCertificateTakesEffect(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	before, err := f.srv.orgs.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// A second stack for the same organisation: new CA, new certificate, new
	// address. Re-registering is what `cloud org register` does on a rotation.
	rotated := newATLStack(t, f.dsn)
	f.registerOrg(t, defaultOrg, rotated)

	// Still the old channel: nothing has re-read the row yet, which is the
	// point of the cache.
	during, err := f.srv.orgs.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("lookup inside the refresh window: %v", err)
	}
	if during != before {
		t.Error("the row was re-read inside the refresh window, so every request " +
			"costs a database round trip")
	}

	// Past the window.
	f.srv.orgs.now = func() time.Time { return time.Now().Add(2 * orgClientRefresh) }

	after, err := f.srv.orgs.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("lookup past the refresh window: %v", err)
	}
	if after == before {
		t.Fatal("the rotated certificate never came into use, so rotating one " +
			"needs a console restart — and the old credential keeps working " +
			"until somebody performs it")
	}
	if after.endpoint != rotated.addr {
		t.Errorf("endpoint: got %s, want %s", after.endpoint, rotated.addr)
	}
	// The replaced channel is closed. Without this each rotation leaks a
	// connection, and nothing observable changes until the process runs out.
	if state := before.client.conn.GetState(); state != connectivity.Shutdown {
		t.Errorf("the replaced channel is %s, not shut down", state)
	}
}

// A database blip does not sign an organisation out of its own atlantis.
func TestADatabaseBlipKeepsTheCachedClient(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	pool := newOrgClients(f.srv.db)
	before, err := pool.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	// A store whose pool is closed: every query fails, which is what an
	// unreachable database looks like from here. Not a deleted row — that is a
	// definitive answer and is tested below.
	broken, err := pgxpool.New(ctx, f.dsn)
	if err != nil {
		t.Fatalf("open a pool to break: %v", err)
	}
	broken.Close()
	pool.db = &store{pool: broken, log: f.srv.db.log, keys: f.srv.db.keys}
	pool.now = func() time.Time { return time.Now().Add(2 * orgClientRefresh) }

	after, err := pool.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("a database blip returned an error instead of the cached client, "+
			"which turns a hiccup into an outage: %v", err)
	}
	if after != before {
		t.Error("the blip replaced the cached client")
	}
}

// A de-provisioned organisation stops being served.
//
// The counterpart to the test above, and the reason the two errors are told
// apart. Treating every read failure as a blip would mean a deleted row keeps
// working until the process restarts — so de-provisioning an organisation
// would appear to do nothing, which is the failure mode this whole step is
// about.
func TestADeprovisionedOrganisationStopsBeingServed(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	before, err := f.srv.orgs.get(ctx, defaultOrg)
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `DELETE FROM console.orgs WHERE org = $1`, defaultOrg); err != nil {
		t.Fatalf("de-provision: %v", err)
	}
	f.srv.orgs.now = func() time.Time { return time.Now().Add(2 * orgClientRefresh) }

	if _, err := f.srv.orgs.get(ctx, defaultOrg); err == nil {
		t.Fatal("a de-provisioned organisation is still being served from cache")
	}
	if state := before.client.conn.GetState(); state != connectivity.Shutdown {
		t.Errorf("the evicted channel is %s, not shut down", state)
	}
}

// The write path consults the validation.
//
// register_test.go proves validateOrgCredentials ranks its inputs correctly,
// which is a different claim from "registerOrg calls it" — and the difference
// is the one this repo keeps paying for. Without this, the call could be
// deleted from store.registerOrg and every test would stay green, leaving the
// validation a pure function nothing reaches.
//
// It matters that the check lives on the store rather than on the exported
// RegisterOrg wrapper: this test writes through the store, as the fixture does,
// so a second door that skipped validation would be visible here.
func TestRegisterOrgRefusesCredentialsThatCannotDial(t *testing.T) {
	f := newConsoleFixture(t)

	bad := f.atl.credentials(t, "initech")
	bad.CertPEM, bad.KeyPEM = string(bad.KeyPEM), []byte(bad.CertPEM)

	err := f.srv.db.registerOrg(context.Background(), bad)
	if err == nil {
		t.Fatal("a certificate and key the wrong way round were written to " +
			"console.orgs. The row looks complete, and the first person to " +
			"open the console gets a 503 about TLS")
	}
	if !strings.Contains(err.Error(), "not a pair") {
		t.Errorf("the error does not say what is wrong with the material: %v", err)
	}

	// And nothing was written, so a failed registration does not leave a
	// half-provisioned organisation behind.
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM console.orgs WHERE org = 'initech'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("the refused registration wrote %d rows", n)
	}
}

// The exported entry point registers an organisation the console can then use.
//
// `cloud org register` calls RegisterOrg, not store.registerOrg, and until this
// existed nothing did. The wrapper opens its own pool and builds its own
// keyring from a base64 string, so it has two ways to be wrong that the store
// method cannot have — and both produce a row that looks complete.
func TestRegisterOrgThroughTheExportedEntryPoint(t *testing.T) {
	f := newConsoleFixture(t)
	ctx := context.Background()

	const org = "initech"
	creds := f.atl.credentials(t, org)
	err := RegisterOrg(ctx, f.consoleDSN, f.keyset, OrgRegistration{
		Org: org, Endpoint: creds.Endpoint, HealthAddr: creds.HealthAddr,
		CAPEM: creds.CAPEM, CertPEM: creds.CertPEM, KeyPEM: creds.KeyPEM,
	})
	if err != nil {
		t.Fatalf("RegisterOrg: %v", err)
	}

	// The console reads it back, which is the claim that matters: registering
	// under a keyset the console does not serve with writes a row that is
	// complete and never opens.
	if _, err := f.srv.db.orgCredentials(ctx, org); err != nil {
		t.Fatalf("the console cannot read what RegisterOrg wrote: %v", err)
	}

	// And a different keyset is refused rather than silently accepted. Without
	// this the test above passes whether or not the keyset argument is used at
	// all.
	other := testKeyset(t)
	if err := RegisterOrg(ctx, f.consoleDSN, other, OrgRegistration{
		Org: org, Endpoint: creds.Endpoint, HealthAddr: creds.HealthAddr,
		CAPEM: creds.CAPEM, CertPEM: creds.CertPEM, KeyPEM: creds.KeyPEM,
	}); err != nil {
		t.Fatalf("register under a second keyset: %v", err)
	}
	if _, err := f.srv.db.orgCredentials(ctx, org); err == nil {
		t.Error("a row sealed under a different keyset opened against the " +
			"console's, so the keyset argument is not reaching the keyring")
	}
}

// Server.Close tears down every organisation's channel.
func TestCloseShutsDownEveryOrganisationsChannel(t *testing.T) {
	f := newTwoOrgFixture(t)
	ctx := context.Background()

	var dialled []*adminClient
	for _, org := range []string{defaultOrg, otherOrg} {
		e, err := f.srv.orgs.get(ctx, org)
		if err != nil {
			t.Fatalf("dial %s: %v", org, err)
		}
		dialled = append(dialled, e.client)
	}
	if len(dialled) != 2 {
		t.Fatalf("dialled %d clients, want 2", len(dialled))
	}

	f.srv.orgs.close()

	for i, c := range dialled {
		if state := c.conn.GetState(); state != connectivity.Shutdown {
			t.Errorf("channel %d is %s after close, not shut down", i, state)
		}
	}
}
