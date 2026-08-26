package console

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Enrolment through the handlers, with a real signer behind real mTLS.
//
// The store tests cover the statement that spends a token. These cover what the
// handlers do around it — and one property nothing else can reach: that the
// caller name sent to the signer comes off the spent row rather than out of the
// request. A console that forwarded the request's caller would return a
// perfectly valid certificate for whatever the requester asked for, and every
// assertion about the response would still pass.

// enrolBody is what a machine posts.
func enrolBody(org, token, csrPEM string) string {
	b, _ := json.Marshal(enrollRequest{Org: org, Token: token, CSRPEM: csrPEM})
	return string(b)
}

// newCSR builds a certificate signing request the way a machine would: it
// generates a key here and never parts with it.
func newCSR(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// enrol posts to the enrolment handler directly.
//
// Not through the enrolment listener: that listener is transport, and what these
// tests are about is the token and the request. Renewal is the one that needs a
// real listener, because a peer certificate is the thing it authenticates by.
func (f *consoleFixture) enrol(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := f.request(t, http.MethodPost, "/enroll", body, "")
	f.srv.handleEnroll(w, r)
	return w
}

// mintToken registers a caller if needed and mints a token for it.
func (f *consoleFixture) mintToken(t *testing.T, caller string) string {
	t.Helper()
	token := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, token)

	reg := f.post(t, "/api/callers", fmt.Sprintf(`{"caller":%q,"can_mutate":true}`, caller), token)
	if reg.Code != http.StatusOK {
		t.Fatalf("register caller: %d %s", reg.Code, reg.Body.String())
	}

	w := f.post(t, "/api/callers/"+caller+"/enroll", "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("mint enrolment token: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if got.Token == "" {
		t.Fatalf("the mint returned no token: %s", w.Body.String())
	}
	return got.Token
}

// TestEnrolmentReturnsACertificateAndNoPrivateKey.
//
// The headline property, asserted on the bytes. Its predecessor generated a
// P-256 key inside the console and returned it as `key_pem`; the whole of K7a
// is that this response cannot contain one, whatever else changes around it.
func TestEnrolmentReturnsACertificateAndNoPrivateKey(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend")))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	for _, forbidden := range []string{"PRIVATE KEY", "key_pem"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the enrolment response contains %q", forbidden)
		}
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
		CAPEM   string `json:"ca_pem"`
		Caller  string `json:"caller"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Caller != "backend" {
		t.Errorf("caller = %q, want backend", got.Caller)
	}
	if !strings.Contains(got.CertPEM, "BEGIN CERTIFICATE") || got.CAPEM == "" {
		t.Errorf("the response is not a certificate and a CA: %+v", got)
	}
}

// TestTheCallerSentToTheSignerComesFromTheTokenRow.
//
// The guard the response cannot show. The signer compares the CSR's common name
// to the caller in the body it was handed — so if the console forwarded a name
// from the request, that comparison would be between two attacker-supplied
// values and would pass. This asserts on what the console SENT.
//
// enrollRequest has no caller field today, which is the strongest form of the
// fix; this test is what makes adding one visible.
func TestTheCallerSentToTheSignerComesFromTheTokenRow(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	if w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend"))); w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	if got := f.signer.lastRequest(t).Caller; got != "backend" {
		t.Errorf("the console asked the signer for %q, want the token row's caller", got)
	}
}

// TestACSRAskingForAnotherCallerGetsTheTokensCaller.
//
// This asserted a refusal. It no longer is one, and the property that replaced
// it is stronger.
//
// The signer names the certificate from the caller it is handed — the console's,
// read off the row it spent the token against — and takes only the public key
// from the request. So a CSR asking to be `payments` on a token minted for
// `backend` does not get refused; it gets a certificate for `backend`, because
// what the CSR asks to be called stopped deciding anything.
//
// Ignoring a field is stronger than comparing it: there is no check left to
// forget, and no configuration in which the comparison could be skipped. It also
// removes a step from the client, which is why `tide login` needs no --caller.
func TestACSRAskingForAnotherCallerGetsTheTokensCaller(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "payments")))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
		Caller  string `json:"caller"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Caller != "backend" {
		t.Errorf("response names caller %q, want the token's", got.Caller)
	}
	// The certificate itself, which is what atlantis authenticates against.
	block, _ := pem.Decode([]byte(got.CertPEM))
	if block == nil {
		t.Fatal("no certificate in the response")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the issued certificate: %v", err)
	}
	if leaf.Subject.CommonName != "backend" {
		t.Errorf("the issued certificate is named %q — a CSR chose its own identity",
			leaf.Subject.CommonName)
	}
	// And the console still sent the token row's caller, which is the guard the
	// response cannot show.
	if sent := f.signer.lastRequest(t).Caller; sent != "backend" {
		t.Errorf("the console asked the signer for %q, want the token row's caller", sent)
	}
}

// TestAnEnrolmentTokenIsSpentEvenWhenTheSignerRefuses.
//
// Deliberate, and worth pinning because the opposite is the tempting choice. A
// token that survived a failed issuance could be replayed against it — and the
// failure modes that reach the signer are ones where something is already
// wrong. The cost is an operator minting another token, which is a button.
func TestAnEnrolmentTokenIsSpentEvenWhenTheSignerRefuses(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")
	f.signer.refuse = "no certificates today"

	if w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend"))); w.Code == http.StatusOK {
		t.Fatalf("the signer refused but enrolment succeeded: %s", w.Body.String())
	}

	f.signer.refuse = ""
	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend")))
	if w.Code == http.StatusOK {
		t.Fatal("the token survived a failed issuance and was redeemed a second time")
	}
}

// TestEnrolmentRecordsTheCertificateItIssued.
//
// This asserted a fingerprint written into atlantis.caller_identities, which
// was what bound a caller to one leaf. Migration 0032 removed that column with
// pinning; what remains is the console's own record, and it is not bookkeeping —
// renewal resolves the organisation and the caller from it, so an enrolment that
// failed to write it produces a certificate that works and can never be renewed.
func TestEnrolmentRecordsTheCertificateItIssued(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend")))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(t.Context(), "backend")
	if err != nil {
		t.Fatalf("the console recorded no certificate, so it could never be renewed: %v", err)
	}
	if !bytes.Equal(rec.Fingerprint, leafFingerprint(t, got.CertPEM)) {
		t.Error("the recorded certificate is not the one that was issued")
	}

	// And atlantis knows when it runs out, which is what the console displays.
	var exp *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT cert_expires_at FROM atlantis.caller_identities WHERE caller = 'backend'`).
		Scan(&exp); err != nil {
		t.Fatalf("read the expiry: %v", err)
	}
	if exp == nil {
		t.Error("enrolment recorded no expiry with atlantis")
	}
}

// TestEnrolmentIsRefusedWithoutAToken, and without the other required fields.
//
// Each of these is a request that reaches an unauthenticated route, so each has
// to be refused by something rather than fall through to a nil check further
// down.
func TestEnrolmentIsRefusedWithoutItsFields(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")
	csr := newCSR(t, "backend")

	for _, tc := range []struct{ name, body string }{
		{"no token", enrolBody(defaultOrg, "", csr)},
		{"no org", enrolBody("", tok, csr)},
		{"no CSR", enrolBody(defaultOrg, tok, "")},
		{"a CSR that is not a CSR", enrolBody(defaultOrg, tok, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----")},
		{"an invented token", enrolBody(defaultOrg, "not-a-token", csr)},
		{"empty body", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := f.enrol(t, tc.body); w.Code == http.StatusOK {
				t.Fatalf("accepted: %s", w.Body.String())
			}
		})
	}

	// The control: the token those cases were built around still works, so none
	// of them passed because the token was already spent.
	if w := f.enrol(t, enrolBody(defaultOrg, tok, csr)); w.Code != http.StatusOK {
		t.Fatalf("the token was consumed by a refused request: %d %s", w.Code, w.Body.String())
	}
}

// TestMintingAnEnrolmentTokenNeedsAdminAndSudo.
//
// It produces a credential that becomes a caller's identity — the same class as
// setting the change policy or revoking every caller.
func TestMintingAnEnrolmentTokenNeedsAdminAndSudo(t *testing.T) {
	f := newEnrolmentFixture(t)

	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)
	if w := f.post(t, "/api/callers", `{"caller":"backend","can_mutate":true}`, admin); w.Code != http.StatusOK {
		t.Fatalf("register caller: %d %s", w.Code, w.Body.String())
	}

	viewer := f.signIn(t, "viewer@example.com", "viewer")
	if w := f.post(t, "/api/callers/backend/enroll", "", viewer); w.Code != http.StatusForbidden {
		t.Errorf("a viewer minted an enrolment token: %d %s", w.Code, w.Body.String())
	}

	// An admin without sudo. Signed in, correct role, and still refused.
	plain := f.signIn(t, "admin2@example.com", "admin")
	if w := f.post(t, "/api/callers/backend/enroll", "", plain); w.Code != http.StatusForbidden {
		t.Errorf("an admin without sudo minted an enrolment token: %d %s", w.Code, w.Body.String())
	}
}

// A token cannot be minted for a caller that does not exist.
//
// The signer refuses this too, from its own database connection — but that
// answer arrives after somebody has carried a token to a machine, which is a
// worse place to discover a typo.
func TestATokenCannotBeMintedForAnUnknownCaller(t *testing.T) {
	f := newEnrolmentFixture(t)
	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)

	w := f.post(t, "/api/callers/never-registered/enroll", "", admin)
	if w.Code != http.StatusNotFound {
		t.Fatalf("minted a token for an unregistered caller: %d %s", w.Code, w.Body.String())
	}
}

// A console with no signer says so, rather than answering 404.
//
// A route answering 503 gives no way to tell an unconfigured console from a
// broken one until the button is pressed. The message is what separates them.
func TestAConsoleWithoutASignerSaysSo(t *testing.T) {
	f := newConsoleFixture(t) // no enrolment
	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)
	if w := f.post(t, "/api/callers", `{"caller":"backend","can_mutate":true}`, admin); w.Code != http.StatusOK {
		t.Fatalf("register caller: %d %s", w.Code, w.Body.String())
	}

	w := f.post(t, "/api/callers/backend/enroll", "", admin)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("the message does not say enrolment is unconfigured: %s", w.Body.String())
	}
}

// newKeyAndCSR builds a CSR and returns the key, for tests where the machine
// keeps it: renewal presents the certificate it was issued.
//
// newCSR above throws the key away, enrolment being about the key never
// leaving. This is the same operation from the machine's side.
func newKeyAndCSR(t *testing.T, cn string) (keyPEM, csrPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestEnrolmentReturnsTheServerCAAndNotTheSignersOwn.
//
// The console keeps two roots per organisation and they answer different
// questions: `console.orgs.ca_pem` is what verifies the organisation's SERVER,
// and the signer's `ca_pem` is whatever issued the CLIENT leaf. register.go says
// in as many words that they "are the same CA in every deployment that exists
// today and are not required to be."
//
// tide uses what it is handed as RootCAs to verify the server. Returning the
// signer's root works in every deployment anyone has tried and breaks the first
// one provisioned with split roots — as a handshake failure at atlantis, with
// nothing pointing back at this line.
//
// The fixture's fake signer issues from the ORGANISATION's authority, so this
// asserts the value came from the console's row rather than from the response.
func TestEnrolmentReturnsTheServerCAAndNotTheSignersOwn(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend")))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}
	var got struct {
		CAPEM     string `json:"ca_pem"`
		Endpoint  string `json:"endpoint"`
		EnrollURL string `json:"enroll_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	creds, err := f.srv.db.orgCredentials(t.Context(), defaultOrg)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	if got.CAPEM != creds.CAPEM {
		t.Error("the CA handed to the machine is not the one that verifies this organisation's server")
	}

	// And the two addresses the certificate cannot carry.
	if got.Endpoint != creds.PublicEndpoint || got.Endpoint == "" {
		t.Errorf("endpoint = %q, want the organisation's caller-facing address %q",
			got.Endpoint, creds.PublicEndpoint)
	}
	if got.EnrollURL != f.srv.cfg.EnrollPublicURL || got.EnrollURL == "" {
		t.Errorf("enroll_url = %q, want %q — renewal is on this listener, not on atlantis",
			got.EnrollURL, f.srv.cfg.EnrollPublicURL)
	}
}

// The caller-facing endpoint falls back to the console's own when unset.
//
// NULL means "the same address", which is true of every organisation registered
// before migration 0008 and every deployment where the console and its callers
// share a route. Stored as NULL rather than backfilled so the two cannot
// silently diverge later.
func TestTheCallerEndpointFallsBackToTheConsolesOwn(t *testing.T) {
	f := newEnrolmentFixture(t)

	creds, err := f.srv.db.orgCredentials(t.Context(), defaultOrg)
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	// The fixture registers without a caller-facing address.
	if creds.PublicEndpoint != creds.Endpoint {
		t.Fatalf("with no override the caller endpoint is %q, want the console's %q",
			creds.PublicEndpoint, creds.Endpoint)
	}

	// Setting one moves it, and only it.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE console.orgs SET atl_public_endpoint = 'callers.example:9090' WHERE org = $1`,
		defaultOrg); err != nil {
		t.Fatalf("set the caller endpoint: %v", err)
	}
	creds, err = f.srv.db.orgCredentials(t.Context(), defaultOrg)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if creds.PublicEndpoint != "callers.example:9090" {
		t.Errorf("caller endpoint = %q, want the override", creds.PublicEndpoint)
	}
	if creds.Endpoint == "callers.example:9090" {
		t.Error("the override moved the console's own endpoint too")
	}
}
