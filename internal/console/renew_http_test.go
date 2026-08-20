package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Renewal, through the listener that production uses.
//
// # Why these do not call the handler directly
//
// Renewal authenticates by the certificate the peer presented, which arrives on
// r.TLS. A test that set r.TLS by hand would pass on any ClientAuth mode the
// listener happened to be configured with — including one that never asks for a
// certificate at all — so it would assert that the handler reads a field, not
// that the property holds. These start the real listener and dial it.

// renewClient dials the enrolment listener presenting certPEM/keyPEM, or
// presenting nothing when both are empty.
func (f *consoleFixture) renewClient(t *testing.T, certPEM, keyPEM string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	caPEM, err := f.srv.db.orgCredentials(context.Background(), defaultOrg)
	if err != nil {
		t.Fatalf("read the organisation's CA: %v", err)
	}
	if !pool.AppendCertsFromPEM([]byte(caPEM.CAPEM)) {
		t.Fatal("the organisation has no usable CA on file")
	}

	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if certPEM != "" {
		pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			t.Fatalf("load the client pair: %v", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
}

// startEnrolListener runs the real listener on a free port and returns its base
// URL.
func (f *consoleFixture) startEnrolListener(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		// ServeTLS rather than ListenAndServeTLS so the port is known before
		// anything dials it.
		_ = f.srv.enrollSrv.ServeTLS(ln, f.srv.cfg.EnrollTLSCert, f.srv.cfg.EnrollTLSKey)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.srv.enrollSrv.Shutdown(ctx)
	})
	return "https://" + ln.Addr().String()
}

// enrolledMachine is what a machine holds after enrolling: its own key, and the
// certificate the console issued for it.
type enrolledMachine struct {
	caller  string
	certPEM string
	keyPEM  string
}

// enrolMachine runs a real enrolment and returns what the machine keeps.
//
// The key is generated here and never leaves, which is the point — so this
// helper has to build the CSR itself rather than borrow newCSR, which throws
// its key away.
func (f *consoleFixture) enrolMachine(t *testing.T, caller string) enrolledMachine {
	t.Helper()

	key, csrPEM := newKeyAndCSR(t, caller)
	tok := f.mintToken(t, caller)

	w := f.enrol(t, enrolBody(defaultOrg, tok, csrPEM))
	if w.Code != http.StatusOK {
		t.Fatalf("enrol %s: %d %s", caller, w.Code, w.Body.String())
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode enrolment: %v", err)
	}
	return enrolledMachine{caller: caller, certPEM: got.CertPEM, keyPEM: key}
}

// renew posts a renewal over the listener.
func renew(t *testing.T, client *http.Client, base, csrPEM string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(renewRequest{CSRPEM: csrPEM})
	resp, err := client.Post(base+"/renew", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestRenewalNeedsTheCertificateItIsReplacing.
//
// No peer certificate at all. The listener is VerifyClientCertIfGiven — it has
// to be, because enrolment arrives without one — so this is the handler's own
// assertion, and it is the whole of renewal's authentication.
func TestRenewalNeedsTheCertificateItIsReplacing(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp := renew(t, f.renewClient(t, "", ""), base, csrPEM)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renewal without a certificate returned %d, want 401", resp.StatusCode)
	}
}

// TestRenewalWorksWithTheCurrentCertificate.
//
// It used to also assert that atlantis's stored fingerprint rotated. Migration
// 0032 removed that column with pinning, so what is checked instead is the
// console's own record — which is what renewal itself resolves from, and so the
// thing whose staleness would actually break something.
func TestRenewalWorksWithTheCurrentCertificate(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	base := f.startEnrolListener(t)

	before, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(context.Background(), "backend")
	if err != nil {
		t.Fatalf("read the current record: %v", err)
	}

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renewal returned %d", resp.StatusCode)
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
		Caller  string `json:"caller"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The organisation and the caller came off the row, not out of the request —
	// the request carried neither.
	if got.Caller != "backend" || got.Org != defaultOrg {
		t.Errorf("renewal resolved to %s/%s", got.Org, got.Caller)
	}

	after, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(context.Background(), "backend")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if bytes.Equal(before.Fingerprint, after.Fingerprint) {
		t.Error("the console still records the old certificate as current")
	}
	if !bytes.Equal(after.Fingerprint, leafFingerprint(t, got.CertPEM)) {
		t.Error("the recorded certificate is not the one just issued")
	}
}

// TestAReplacedCertificateCanStillRenew.
//
// The failure this exists to remove: a renewal records the new certificate
// before the machine can possibly have stored it, so if the response is lost the
// machine is still holding the old one — and needs a valid certificate to try
// again.
//
// This used to depend on a 24-hour overlap window, which existed because
// atlantis pinned a caller to one leaf. Migration 0032 removed the pinning and
// with it the window: an older certificate is simply still that caller's
// certificate until it expires, so the recovery below needs no special case at
// all. That is the simplification a seven-day lifetime bought.
func TestAReplacedCertificateCanStillRenew(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	base := f.startEnrolListener(t)

	// Renew, and throw the response away — the lost-response case exactly.
	_, csrPEM := newKeyAndCSR(t, "backend")
	if resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM); resp.StatusCode != http.StatusOK {
		t.Fatalf("first renewal: %d", resp.StatusCode)
	}

	// The machine, still holding the certificate it started with, retries.
	_, csr2 := newKeyAndCSR(t, "backend")
	resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csr2)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a machine holding the superseded certificate could not retry: %d", resp.StatusCode)
	}
}

// TestACertificateThisConsoleDidNotIssueCannotRenew.
//
// A perfectly valid certificate from the organisation's own authority, which
// this console never recorded — `make dev-caller-cert` produces exactly this.
// It authenticates at atlantis and is not renewable here, because the console
// has no way to know whose it is.
func TestACertificateThisConsoleDidNotIssueCannotRenew(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)

	// Issued by the organisation's CA directly, bypassing enrolment.
	certFile, keyFile := f.atl.pki.ClientCert(t, "backend")
	certPEM, keyPEM := readFileString(t, certFile), readFileString(t, keyFile)

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp := renew(t, f.renewClient(t, certPEM, keyPEM), base, csrPEM)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an unrecorded certificate renewed: %d", resp.StatusCode)
	}
}

// TestRenewalCannotChangeWhichCallerYouAre.
//
// A machine renews with a CSR naming a different caller. It is not refused —
// the signer names the leaf from the caller the console resolved, so the CSR's
// subject decides nothing — and what comes back is a certificate for the caller
// it already was.
//
// The assertion used to be "refused". That was weaker: it tested a comparison
// somebody could remove, where this tests that the identity in the issued
// certificate never came from the request at all.
func TestRenewalCannotChangeWhichCallerYouAre(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	// A second caller exists, so nothing here turns on "payments is unknown".
	f.mintToken(t, "payments")
	base := f.startEnrolListener(t)

	_, csrPEM := newKeyAndCSR(t, "payments")
	resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: %d", resp.StatusCode)
	}
	var got struct {
		CertPEM string `json:"cert_pem"`
		Caller  string `json:"caller"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Caller != "backend" {
		t.Errorf("renewal resolved to caller %q, want backend", got.Caller)
	}
	block, _ := pem.Decode([]byte(got.CertPEM))
	if block == nil {
		t.Fatal("no certificate in the response")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if leaf.Subject.CommonName != "backend" {
		t.Errorf("renewal issued a certificate for %q — a CSR chose its own identity",
			leaf.Subject.CommonName)
	}

	// And payments is untouched: it was never enrolled, so this console holds no
	// certificate for it.
	if fp := f.callerFingerprint(t, "payments"); len(fp) != 0 {
		t.Error("the renewal recorded a certificate against payments")
	}
}

// A renewal is audited with both fingerprints.
//
// It is the one event where two certificates are briefly valid for one caller,
// so a trail naming only the new one cannot answer "which certificate was
// this?" for anything that happened during the overlap.
func TestRenewalIsAuditedWithBothFingerprints(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	base := f.startEnrolListener(t)

	old := hex.EncodeToString(f.callerFingerprint(t, "backend"))

	_, csrPEM := newKeyAndCSR(t, "backend")
	if resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM); resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: %d", resp.StatusCode)
	}

	var detail string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT detail::text FROM console.audit_log
		 WHERE action = 'certificate_renewed' ORDER BY created_at DESC LIMIT 1
	`).Scan(&detail); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if !strings.Contains(detail, old) {
		t.Errorf("the audit row does not name the certificate that was replaced: %s", detail)
	}
	newFP := hex.EncodeToString(f.callerFingerprint(t, "backend"))
	if !strings.Contains(detail, newFP) {
		t.Errorf("the audit row does not name the new certificate: %s", detail)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// callerFingerprint reads the certificate this console currently records for a
// caller, or nil when it has never enrolled one.
//
// It used to read atlantis.caller_identities.cert_fingerprint, which migration
// 0032 dropped — and it swallowed the resulting error and returned nil, so
// every assertion built on it passed without checking anything. Worth naming:
// a helper that returns a zero value on error turns three tests into no tests.
func (f *consoleFixture) callerFingerprint(t *testing.T, caller string) []byte {
	t.Helper()
	rec, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(context.Background(), caller)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the recorded certificate for %s: %v", caller, err)
	}
	return rec.Fingerprint
}

func leafFingerprint(t *testing.T, certPEM string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("not a PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return sum[:]
}
