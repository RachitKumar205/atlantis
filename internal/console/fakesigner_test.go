package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/testsupport/testpki"
)

// A stand-in for cmd/signer, over real mTLS.
//
// It fakes the certificate authority, since signing a CSR needs a CA private
// key and reaching for the real one would test cmd/signer rather than the
// console. Everything else is real: real TLS, real client certificate
// verification, real X.509 issued from a real request.
//
// It generates no keypair. Enrolment's claim is that the private key stays on
// the machine that made the request, and a stand-in minting its own key
// produces an identical response that nothing downstream checks.
type fakeSigner struct {
	URL string

	// pki is the authority that decides who may call this signer, and is also
	// its own server identity. Separate from the one it issues from; see the
	// fixture for what that separation buys.
	pki                   *testpki.PKI
	clientCert, clientKey string

	// issuing signs the certificates handed back. It is the organisation's own
	// authority, because that is what the organisation's atlantis trusts.
	issuing *testpki.PKI

	// advertisedCAPEM is what this signer REPORTS as ca_pem, which is not the
	// authority it signs with. See handle() for why they are made to differ.
	advertisedCAPEM string

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
		// The client-trust PKI's root: a real certificate, and not the one the
		// organisation's server is verified against.
		advertisedCAPEM: readFileString(t, clientPKI.CAFile),
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

	// req.Caller names the leaf, exactly as cmd/signer does.
	certPEM, caPEM, err := f.issuing.SignCSR(req.CSRPEM, req.Caller)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Report a DIFFERENT root than the one the leaf chains to.
	//
	// Artificial. In every deployment the signer's root and the organisation's
	// server root are the same bytes, so a test cannot tell whether the console
	// echoes the signer's `ca_pem` or reads its own `console.orgs.ca_pem`: both
	// produce an identical response, and a mutation swapping one for the other
	// survives.
	//
	// register.go records that the two "are the same CA in every deployment that
	// exists today and are not required to be". This makes them differ so the
	// question has an answer: the machine must be handed the root that verifies
	// the SERVER, and the signer's is the one that issued its CLIENT leaf.
	//
	// The leaf still chains to the organisation's authority, so verifyLeafForOrg
	// is unaffected — only the advertised root differs.
	_ = caPEM
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"cert_pem":   certPEM,
		"ca_pem":     f.advertisedCAPEM,
		"expires_at": "",
	})
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
