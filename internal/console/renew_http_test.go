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

// TestRenewalWorksWithTheCurrentCertificate, and rotates the binding.
func TestRenewalWorksWithTheCurrentCertificate(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	base := f.startEnrolListener(t)

	before := f.callerFingerprint(t, "backend")

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

	after := f.callerFingerprint(t, "backend")
	if bytes.Equal(before, after) {
		t.Error("the binding still names the old certificate")
	}
	if want := leafFingerprint(t, got.CertPEM); !bytes.Equal(after, want) {
		t.Error("the binding does not name the certificate that was just issued")
	}
}

// TestTheReplacedCertificateKeepsWorkingForItsWindow.
//
// The failure this whole step exists to remove. A renewal records the new
// fingerprint before the machine can have stored the certificate, so if the
// response is lost the machine is still holding the old one — and needs a valid
// certificate to try again.
func TestTheReplacedCertificateKeepsWorkingForItsWindow(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	base := f.startEnrolListener(t)

	old := f.callerFingerprint(t, "backend")

	// Renew, and throw the response away — the lost-response case exactly.
	_, csrPEM := newKeyAndCSR(t, "backend")
	if resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM); resp.StatusCode != http.StatusOK {
		t.Fatalf("first renewal: %d", resp.StatusCode)
	}

	// atlantis now names the new certificate, and remembers the old one.
	prev, until := f.callerPrevious(t, "backend")
	if !bytes.Equal(prev, old) {
		t.Fatal("the replaced certificate was not recorded as the previous one")
	}
	if until.Before(time.Now()) {
		t.Fatalf("the overlap window is already over: %s", until)
	}

	// And the machine, still holding the old certificate, can renew again.
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
// The CSR names a different caller. The console takes the caller from the row
// the fingerprint resolved to, so the CN check refuses it — a machine cannot
// renew its way into somebody else's identity.
func TestRenewalCannotChangeWhichCallerYouAre(t *testing.T) {
	f := newEnrolmentFixture(t)
	m := f.enrolMachine(t, "backend")
	// A second caller exists, so the refusal is not "payments is unknown".
	f.mintToken(t, "payments")
	base := f.startEnrolListener(t)

	_, csrPEM := newKeyAndCSR(t, "payments")
	resp := renew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("backend renewed itself into payments")
	}
	if fp := f.callerFingerprint(t, "payments"); len(fp) != 0 {
		t.Error("the refused renewal still rebound payments")
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

// callerFingerprint reads what atlantis currently binds a caller to.
func (f *consoleFixture) callerFingerprint(t *testing.T, caller string) []byte {
	t.Helper()
	var fp []byte
	err := f.pool.QueryRow(context.Background(),
		`SELECT cert_fingerprint FROM atlantis.caller_identities WHERE caller = $1`,
		caller).Scan(&fp)
	if err != nil {
		return nil
	}
	return fp
}

// callerPrevious reads the overlap slot.
func (f *consoleFixture) callerPrevious(t *testing.T, caller string) ([]byte, time.Time) {
	t.Helper()
	var (
		fp    []byte
		until *time.Time
	)
	if err := f.pool.QueryRow(context.Background(),
		`SELECT prev_cert_fingerprint, prev_valid_until FROM atlantis.caller_identities WHERE caller = $1`,
		caller).Scan(&fp, &until); err != nil {
		t.Fatalf("read the previous fingerprint: %v", err)
	}
	if until == nil {
		return fp, time.Time{}
	}
	return fp, *until
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
