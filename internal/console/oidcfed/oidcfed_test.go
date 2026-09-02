package oidcfed

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
)

// fakeIssuer publishes discovery and keys the way a real OIDC issuer does,
// and mints tokens with its private key.
type fakeIssuer struct {
	srv  *httptest.Server
	key  *ecdsa.PrivateKey
	kid  string
	name string // the issuer the discovery document claims to be
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key, kid: "test-key"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		name := f.name
		if name == "" {
			name = f.srv.URL
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   name,
			"jwks_uri": f.srv.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: f.kid, Algorithm: "ES256", Use: "sig",
		}}})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIssuer) mint(t *testing.T, iss, aud, sub string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: f.key, KeyID: f.kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: iss, Subject: sub, Audience: jwt.Audience{aud},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(exp),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// verifierFor trusts the fake issuer's TLS certificate; production trusts
// the system roots.
func verifierFor(f *fakeIssuer) *Verifier {
	v := New()
	v.client = f.srv.Client()
	return v
}

func TestVerifyAcceptsAMatchingToken(t *testing.T) {
	f := newFakeIssuer(t)
	v := verifierFor(f)

	tok := f.mint(t, f.srv.URL, "atlantis-enroll", "repo:acme/api:ref:refs/heads/main",
		time.Now().Add(time.Minute))
	id, err := v.Verify(context.Background(), tok, f.srv.URL, "atlantis-enroll", "repo:acme/api:*")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if id.Subject != "repo:acme/api:ref:refs/heads/main" {
		t.Errorf("subject = %q", id.Subject)
	}
}

func TestVerifyRefusesEveryMismatch(t *testing.T) {
	f := newFakeIssuer(t)
	v := verifierFor(f)
	good := func() string {
		return f.mint(t, f.srv.URL, "atlantis-enroll", "repo:acme/api:ref:refs/heads/main",
			time.Now().Add(time.Minute))
	}

	for _, tc := range []struct {
		name          string
		token         string
		iss, aud, pat string
	}{
		{"wrong audience", good(), f.srv.URL, "someone-else", "repo:acme/api:*"},
		{"wrong issuer claim", f.mint(t, "https://evil.example", "atlantis-enroll",
			"repo:acme/api:x", time.Now().Add(time.Minute)), f.srv.URL, "atlantis-enroll", "repo:acme/api:*"},
		{"expired", f.mint(t, f.srv.URL, "atlantis-enroll", "repo:acme/api:x",
			time.Now().Add(-2*time.Minute)), f.srv.URL, "atlantis-enroll", "repo:acme/api:*"},
		{"subject outside the pattern", good(), f.srv.URL, "atlantis-enroll", "repo:globex/*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tc.token, tc.iss, tc.aud, tc.pat); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// The discovery document must claim to be the issuer the rule names. A page
// that answers discovery for someone else's identity is exactly the thing
// RFC 8414's rule exists for.
func TestDiscoveryIssuerMismatchIsRefused(t *testing.T) {
	f := newFakeIssuer(t)
	f.name = "https://someone-else.example"
	v := verifierFor(f)

	tok := f.mint(t, f.srv.URL, "atlantis-enroll", "repo:acme/api:x", time.Now().Add(time.Minute))
	if _, err := v.Verify(context.Background(), tok, f.srv.URL, "atlantis-enroll", "repo:acme/*"); err == nil {
		t.Error("a lying discovery document was accepted")
	}
}

func TestMatchSubject(t *testing.T) {
	for _, tc := range []struct {
		pattern, sub string
		want         bool
	}{
		{"repo:acme/api:*", "repo:acme/api:ref:refs/heads/main", true},
		{"repo:acme/api:*", "repo:acme/apix:ref:x", false},
		{"repo:acme/api:ref:refs/heads/main", "repo:acme/api:ref:refs/heads/main", true},
		{"repo:acme/api:ref:refs/heads/main", "repo:acme/api:ref:refs/heads/dev", false},
		{"repo:acme/*:environment:*", "repo:acme/api:environment:prod", true},
		{"repo:acme/*:environment:*", "repo:acme/api:ref:refs/heads/main", false},
		// '*' crosses ':' — the universal GitHub pattern depends on it.
		{"repo:acme/api:*", "repo:acme/api:environment:prod", true},
	} {
		if got := MatchSubject(tc.pattern, tc.sub); got != tc.want {
			t.Errorf("MatchSubject(%q, %q) = %v", tc.pattern, tc.sub, got)
		}
	}
}

func TestValidatePatternRefusesTheBroadShapes(t *testing.T) {
	for _, bad := range []string{"", "*", "*:*", "acme*", "*acme:x", "repo*", "repo:*"} {
		if err := ValidatePattern(bad); err == nil {
			t.Errorf("pattern %q was accepted", bad)
		}
	}
	for _, good := range []string{"repo:acme/api:*", "repo:acme/api:ref:refs/heads/main", "repo:acme/*"} {
		if err := ValidatePattern(good); err != nil {
			t.Errorf("pattern %q refused: %v", good, err)
		}
	}
}
