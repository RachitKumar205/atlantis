package console

import (
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

// TestACSRForAnotherCallerIsRefused.
//
// A token minted for `backend`, a request asking for `payments`. Refused before
// the signer is called at all — the console does not rely on the signer's own
// common-name check, which cannot see the token.
func TestACSRForAnotherCallerIsRefused(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	before := f.signer.callCount()
	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "payments")))
	if w.Code == http.StatusOK {
		t.Fatalf("a CSR for another caller was signed: %s", w.Body.String())
	}
	if f.signer.callCount() != before {
		t.Error("the console asked the signer for a certificate it should have refused itself")
	}
	if !strings.Contains(w.Body.String(), "payments") {
		t.Errorf("the refusal does not say what was asked for: %s", w.Body.String())
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

// TestEnrolmentRecordsTheFingerprintWithAtlantis.
//
// The write that decides whether the certificate authenticates at all. Its
// absence is silent: the machine gets a certificate, stores it, and is refused
// on the first RPC with "cert superseded" — long after anyone would connect the
// two events.
func TestEnrolmentRecordsTheFingerprintWithAtlantis(t *testing.T) {
	f := newEnrolmentFixture(t)
	tok := f.mintToken(t, "backend")

	w := f.enrol(t, enrolBody(defaultOrg, tok, newCSR(t, "backend")))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol: %d %s", w.Code, w.Body.String())
	}

	var fp []byte
	if err := f.pool.QueryRow(t.Context(),
		`SELECT cert_fingerprint FROM atlantis.caller_identities WHERE caller = 'backend'`).
		Scan(&fp); err != nil {
		t.Fatalf("read the fingerprint: %v", err)
	}
	if len(fp) == 0 {
		t.Fatal("enrolment left the caller unbound, so any CA-signed certificate still authenticates")
	}

	// And the console's own record agrees, which is what renewal will read.
	rec, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(t.Context(), "backend")
	if err != nil {
		t.Fatalf("the console recorded no certificate: %v", err)
	}
	if string(rec.Fingerprint) != string(fp) {
		t.Error("the console and atlantis disagree about which certificate is current")
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
// Its predecessor was configured in no deployment that ever ran and answered
// 503 on a route nobody could tell was unconfigured until they pressed it. The
// message is the difference between "we have not set this up" and "this console
// is broken".
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

// newKeyAndCSR builds a CSR and RETURNS the key, for tests that need the
// machine to keep it — renewal presents the certificate it was issued.
//
// newCSR above throws the key away on purpose, because enrolment is about the
// key never leaving; this is the same operation from the machine's side.
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
