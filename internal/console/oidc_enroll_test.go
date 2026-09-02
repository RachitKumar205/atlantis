package console

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/rachitkumar205/atlantis/internal/console/oidcfed"
)

// A fake OIDC issuer the enrolment listener can verify against: discovery,
// keys, and tokens minted with its private key.
type fakeOIDCIssuer struct {
	srv *httptest.Server
	key *ecdsa.PrivateKey
}

func newFakeOIDCIssuer(t *testing.T) *fakeOIDCIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDCIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "ci-key", Algorithm: "ES256", Use: "sig",
		}}})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOIDCIssuer) mint(t *testing.T, aud, sub string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: f.key, KeyID: "ci-key"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: f.srv.URL, Subject: sub, Audience: jwt.Audience{aud},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(5 * time.Minute)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// oidcEnrol posts to the workload arm directly, as f.enrol does for /enroll.
func (f *consoleFixture) oidcEnrol(t *testing.T, org, caller, idToken, csrPEM string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(oidcEnrollRequest{Org: org, Caller: caller, IDToken: idToken, CSRPEM: csrPEM})
	w := httptest.NewRecorder()
	r := f.request(t, http.MethodPost, "/enroll/oidc", string(b), "")
	f.srv.handleEnrollOIDC(w, r)
	return w
}

// federate registers a caller and one rule for it, and points the verifier at
// the fake issuer's TLS.
func (f *consoleFixture) federate(t *testing.T, issuer *fakeOIDCIssuer, caller, pattern string, budget int) {
	t.Helper()
	f.registerCaller(t, caller)
	f.srv.oidc = oidcfed.NewWithClient(issuer.srv.Client())
	if _, err := f.srv.db.forOrg(defaultOrg).addFederationRule(context.Background(), federationRule{
		Caller: caller, IssuerURL: issuer.srv.URL, Audience: "atlantis-enroll",
		SubjectPattern: pattern, RenewalBudget: budget, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAWorkloadTokenEnrolsUnderARule(t *testing.T) {
	f := newEnrolmentFixture(t)
	issuer := newFakeOIDCIssuer(t)
	f.federate(t, issuer, "ci", "repo:acme/api:*", 1)

	tok := issuer.mint(t, "atlantis-enroll", "repo:acme/api:ref:refs/heads/main")
	w := f.oidcEnrol(t, defaultOrg, "ci", tok, newCSR(t, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("oidc enrol: %d %s", w.Code, w.Body.String())
	}

	// The console asked the signer for the short life, and recorded the
	// rule's renewal budget against the certificate.
	if got := f.signer.lastRequest(t); got.TTLSeconds != oidcCertTTLSeconds {
		t.Errorf("the signer was asked for ttl %d, want %d", got.TTLSeconds, oidcCertTTLSeconds)
	}
	rec, err := f.srv.db.forOrg(defaultOrg).currentCallerCert(context.Background(), "ci")
	if err != nil {
		t.Fatal(err)
	}
	full, err := f.srv.db.callerCertByFingerprint(context.Background(), rec.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if full.RenewalsRemaining == nil || *full.RenewalsRemaining != 1 {
		t.Errorf("renewals_remaining = %v, want the rule's budget", full.RenewalsRemaining)
	}
}

func TestAWorkloadTokenOutsideEveryRuleIsRefused(t *testing.T) {
	f := newEnrolmentFixture(t)
	issuer := newFakeOIDCIssuer(t)
	f.federate(t, issuer, "ci", "repo:acme/api:*", 1)

	for name, tok := range map[string]string{
		"wrong subject":  issuer.mint(t, "atlantis-enroll", "repo:evil/repo:ref:x"),
		"wrong audience": issuer.mint(t, "someone-else", "repo:acme/api:ref:x"),
	} {
		if w := f.oidcEnrol(t, defaultOrg, "ci", tok, newCSR(t, "")); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	// A caller with no rules at all answers the same way.
	f.registerCaller(t, "ruleless")
	tok := issuer.mint(t, "atlantis-enroll", "repo:acme/api:ref:x")
	if w := f.oidcEnrol(t, defaultOrg, "ruleless", tok, newCSR(t, "")); w.Code != http.StatusForbidden {
		t.Errorf("no rules: %d", w.Code)
	}
}

// A workload's certificate renews within its budget and not past it, and the
// successor keeps the short life rather than inheriting the signer's default.
func TestAWorkloadCertificateRenewsWithinItsBudget(t *testing.T) {
	f := newEnrolmentFixture(t)
	issuer := newFakeOIDCIssuer(t)
	f.federate(t, issuer, "ci", "repo:acme/api:*", 1)

	key, csrPEM := newKeyAndCSR(t, "")
	tok := issuer.mint(t, "atlantis-enroll", "repo:acme/api:ref:refs/heads/main")
	w := f.oidcEnrol(t, defaultOrg, "ci", tok, csrPEM)
	if w.Code != http.StatusOK {
		t.Fatalf("oidc enrol: %d %s", w.Code, w.Body.String())
	}
	var first struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	base := f.startEnrolListener(t)

	renewKey, renewCSR := newKeyAndCSR(t, "")
	resp := renew(t, f.renewClient(t, first.CertPEM, key), base, renewCSR)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first renewal: %d", resp.StatusCode)
	}
	var second struct {
		CertPEM string `json:"cert_pem"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		t.Fatal(err)
	}
	// The successor asked the signer for the presented leaf's own life, not
	// the 7-day default the plain renewal path implies.
	if got := f.signer.lastRequest(t); got.TTLSeconds <= 0 {
		t.Errorf("the renewal asked for ttl %d; a workload cert renewed into the full term", got.TTLSeconds)
	}

	// The budget is spent. The successor holds zero renewals and is refused.
	_, exhaustedCSR := newKeyAndCSR(t, "")
	resp2 := renew(t, f.renewClient(t, second.CertPEM, renewKey), base, exhaustedCSR)
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("second renewal answered %d, want a refusal naming the budget", resp2.StatusCode)
	}
}
