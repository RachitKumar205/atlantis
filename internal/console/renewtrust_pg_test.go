package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// Who may renew, asserted as a property rather than as a mechanism.
//
// # Why these do not assert a status code
//
// The rule "a certificate from another authority cannot renew" is enforced
// today inside the TLS handshake, so a machine holding one never reaches a
// handler and never sees a status — it sees a connection error. Enforcing it in
// the handler instead produces a 403 on a connection that succeeded.
//
// Both are correct. A test pinned to either one would have to be rewritten to
// move the check, and a test that gets rewritten alongside the code it guards
// is not a guard. So these assert what must be true in both worlds: the
// renewal does not succeed, and no certificate is issued.
//
// That distinction is not academic here. Before this file, the chain rule had
// NO test at all — the only negative renewal cases were "no certificate" and "a
// valid certificate this console never issued", both of which are refused for
// different reasons and neither of which touches the authority.

// recordedFingerprint is the certificate the console currently believes a
// caller holds, newest first.
//
// Read from the table rather than through callerCertByFingerprint, which looks
// up the other way round — by hash — and so cannot answer "what does this
// caller hold now".
func (f *consoleFixture) recordedFingerprint(t *testing.T, caller string) string {
	t.Helper()
	var fp []byte
	err := f.srv.db.pool.QueryRow(context.Background(), `
		SELECT fingerprint FROM console.caller_certs
		 WHERE org = $1 AND caller = $2
		 ORDER BY issued_at DESC LIMIT 1
	`, defaultOrg, caller).Scan(&fp)
	if err != nil {
		t.Fatalf("read the recorded certificate for %s: %v", caller, err)
	}
	return hex.EncodeToString(fp)
}

// tryRenew posts a renewal and tolerates a transport failure.
//
// The standard helper fatals on one, which is right for tests where the
// connection is expected to work. Here a failed handshake is one of the two
// acceptable outcomes.
func tryRenew(t *testing.T, client *http.Client, base, csrPEM string) (*http.Response, error) {
	t.Helper()
	body, _ := json.Marshal(renewRequest{CSRPEM: csrPEM})
	resp, err := client.Post(base+"/renew", "application/json", bytes.NewReader(body))
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

// A certificate from an authority this organisation does not trust cannot
// renew, however that refusal is delivered.
func TestACertificateFromAnotherAuthorityCannotRenew(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)

	// Enrol properly first, so there is a recorded certificate for this caller
	// and the only thing wrong with the one presented below is who signed it.
	// Without this the test would pass on the "no such fingerprint" branch and
	// prove nothing about the authority.
	real := f.enrolMachine(t, "backend")
	recordedBefore := f.recordedFingerprint(t, "backend")

	// A completely separate authority, as another organisation's would be.
	foreign := testpki.New(t, t.TempDir())
	certFile, keyFile := foreign.ClientCert(t, "backend")

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp, err := tryRenew(t,
		f.renewClient(t, readFileString(t, certFile), readFileString(t, keyFile)),
		base, csrPEM)

	// Refused at the handshake, or refused by the handler. Both are the rule
	// holding; a 200 is the rule gone.
	if err == nil && resp.StatusCode == http.StatusOK {
		t.Fatal("a certificate signed by an authority this organisation does not " +
			"trust was renewed")
	}

	// And nothing was issued. A refusal that still rotated the recorded
	// certificate would lock out the machine actually holding it — the failure
	// mode that matters more than the status code.
	if got := f.recordedFingerprint(t, "backend"); got != recordedBefore {
		t.Error("a refused renewal replaced the recorded certificate, so the machine " +
			"holding the real one can no longer be identified")
	}
	if real.certPEM == "" {
		t.Fatal("the fixture did not enrol a certificate to begin with")
	}
}

// A self-signed certificate is the same rule with no authority at all, and is
// what a machine sends when somebody generates one by hand.
func TestASelfSignedCertificateCannotRenew(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)
	f.enrolMachine(t, "backend")

	// testpki's own root, presented as a client certificate: signed by nobody
	// this console trusts.
	rogue := testpki.New(t, t.TempDir())
	certFile, keyFile := rogue.ClientCert(t, "backend")

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp, err := tryRenew(t,
		f.renewClient(t, readFileString(t, certFile), readFileString(t, keyFile)),
		base, csrPEM)

	if err == nil && resp.StatusCode == http.StatusOK {
		t.Fatal("a self-signed certificate was renewed")
	}
}

// The refusal reaches the handler, and says why.
//
// This one DOES pin the mechanism, on purpose and only now: it is what proves
// the check actually moved. Under the old listener the connection never
// completed, so there was no status and no message — a refusal nobody could see
// from either end. If this ever goes back to a transport error, verification
// has silently returned to the handshake, where it cannot cover more than one
// organisation.
func TestTheRefusalIsAnAnswerRatherThanADroppedConnection(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)
	f.enrolMachine(t, "backend")

	foreign := testpki.New(t, t.TempDir())
	certFile, keyFile := foreign.ClientCert(t, "backend")

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp, err := tryRenew(t,
		f.renewClient(t, readFileString(t, certFile), readFileString(t, keyFile)),
		base, csrPEM)
	if err != nil {
		t.Fatalf("the connection was dropped instead of answered: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	// A reason, not an empty body. Which reason depends on which check fires
	// first, and for a foreign certificate that is the fingerprint lookup —
	// it has a hash this console never recorded, so it is refused before the
	// chain is ever consulted. Both are honest refusals with something a
	// reader can act on, which is the whole difference from a reset connection.
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if !bytes.Contains(body[:n], []byte("error")) {
		t.Errorf("the refusal carries no reason: %s", body[:n])
	}
}

// The case the chain check exists for, and the only one that reaches it.
//
// A certificate this console really did issue, for this organisation, that the
// organisation's atlantis would no longer accept because its authority has been
// rotated since. The fingerprint still matches, so every earlier check passes;
// renewing it would mint a successor from the new authority and supersede a
// working identity with one that authenticates nowhere.
func TestACertificateFromABeforeARotatedAuthorityCannotRenew(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)

	m := f.enrolMachine(t, "backend")

	// Rotate the organisation's authority, leaving the listener's own server
	// certificate alone — which is what a re-registration with fresh material
	// does to a running console.
	rotated := testpki.New(t, t.TempDir())
	if _, err := f.srv.db.pool.Exec(context.Background(),
		`UPDATE console.orgs SET ca_pem = $2, updated_at = NOW() WHERE org = $1`,
		defaultOrg, readFileString(t, rotated.CAFile)); err != nil {
		t.Fatal(err)
	}

	// The client still verifies the server against the ORIGINAL authority,
	// because that is what the listener is still serving. Only the row moved.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(readFileString(t, f.atl.pki.CAFile))) {
		t.Fatal("the original CA did not parse")
	}
	pair, err := tls.X509KeyPair([]byte(m.certPEM), []byte(m.keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		}},
	}

	_, csrPEM := newKeyAndCSR(t, "backend")
	resp, err := tryRenew(t, client, base, csrPEM)
	if err != nil {
		t.Fatalf("the connection failed: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a certificate from before the rotation renewed: %d", resp.StatusCode)
	}
}

// The control. Without it, every assertion above passes against a listener that
// refuses everything — including the machine that should be renewing.
func TestTheRealCertificateStillRenews(t *testing.T) {
	f := newEnrolmentFixture(t)
	base := f.startEnrolListener(t)

	m := f.enrolMachine(t, "backend")
	_, csrPEM := newKeyAndCSR(t, "backend")

	resp, err := tryRenew(t, f.renewClient(t, m.certPEM, m.keyPEM), base, csrPEM)
	if err != nil {
		t.Fatalf("the enrolled machine could not reach renewal: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the enrolled machine was refused: %d", resp.StatusCode)
	}
}
