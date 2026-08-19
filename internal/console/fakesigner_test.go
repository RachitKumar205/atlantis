package console

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// A stand-in for cmd/signer, over real mTLS.
//
// # What it fakes and what it does not
//
// It fakes the certificate authority — signing a CSR needs a CA private key,
// and a test that reached for the real one would be a test of cmd/signer rather
// than of the console. Everything else is real: real TLS, real client
// certificate verification, real X.509 issued from a real request.
//
// In particular it does NOT generate a keypair. The whole claim of enrolment is
// that the private key stays on the machine that made the request, so a
// stand-in that quietly minted its own key would let the console pass a test it
// ought to fail — the response would look identical and nothing downstream
// checks.
type fakeSigner struct {
	URL string

	// pki is the authority that decides who may CALL this signer, and is also
	// its own server identity. Separate from the one it issues from; see the
	// fixture for why that separation is the whole point.
	pki                   *testpki.PKI
	clientCert, clientKey string

	// issuing signs the certificates handed back. It is the organisation's own
	// authority, because that is what the organisation's atlantis trusts.
	issuing *testpki.PKI

	mu sync.Mutex
	// requests records what the console actually sent, so a test can assert on
	// the caller name rather than only on what came back. The CN-to-token match
	// is invisible in the response: a console that forwarded the request's
	// caller instead of the token row's would return a perfectly good
	// certificate.
	requests []signerRequest

	// refuse, when set, makes the next issuance fail. For the paths where the
	// signer says no after the token has already been spent.
	refuse string
}

type signerRequest struct {
	Caller string `json:"caller"`
	CSRPEM string `json:"csr_pem"`
}

// newFakeSigner starts one. issuing is the authority it signs with.
func newFakeSigner(t *testing.T, issuing *testpki.PKI) *fakeSigner {
	t.Helper()

	clientPKI := testpki.New(t, t.TempDir())
	certFile, keyFile := clientPKI.ClientCert(t, "atlantis-console")

	f := &fakeSigner{
		pki: clientPKI, clientCert: certFile, clientKey: keyFile,
		issuing: issuing,
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.handle))
	// RequireAndVerifyClientCert, so a console that failed to present its
	// certificate is refused in the handshake rather than served.
	srv.TLS = clientPKI.ServerTLS(t)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	f.URL = srv.URL
	return f
}

func (f *fakeSigner) handle(w http.ResponseWriter, r *http.Request) {
	var req signerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	refuse := f.refuse
	f.mu.Unlock()

	if refuse != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": refuse})
		return
	}

	certPEM, caPEM, err := f.issuing.SignCSR(req.CSRPEM)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// The real signer also refuses a CSR whose common name is not the caller in
	// the body. Kept here rather than left out, because a console that stopped
	// checking would otherwise look correct against a stand-in that had also
	// stopped — and this is the one guard the response cannot reveal.
	if csrCN(certPEM) != req.Caller {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "CSR CN does not match the caller",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"cert_pem":   certPEM,
		"ca_pem":     caPEM,
		"expires_at": "",
	})
}

// csrCN reads the common name back off an issued certificate.
func csrCN(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return ""
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	return c.Subject.CommonName
}

// lastRequest returns what the console most recently asked for.
func (f *fakeSigner) lastRequest(t *testing.T) signerRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the console never called the signer")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeSigner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}
