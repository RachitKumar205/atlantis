package issuer

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

func mustKey(t *testing.T) *Key {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func validGrant() Grant {
	return Grant{
		Subject:  "usr_01HQ",
		Org:      "acme",
		Role:     identity.RoleAdmin,
		Email:    "rachit@example.com",
		Name:     "Rachit Kumar",
		Audience: "https://acme.console.atlantis.dev",
	}
}

// TestJWKSPublishesNoPrivateKeyMaterial is the one failure in this package
// that would be unrecoverable. Publishing the signing key lets anyone mint an
// assertion for any user in any organisation, and because the tokens would
// verify perfectly there is nothing downstream that could notice.
//
// The assertion is made against the serialised document rather than against
// the Go value, because that is what actually leaves the process — a JWK
// marshals whatever key it was handed, so the guarantee has to be that the
// private half was never put in it.
func TestJWKSPublishesNoPrivateKeyMaterial(t *testing.T) {
	active, retired := mustKey(t), mustKey(t)
	iss, err := New("https://cloud.atlantis.dev", active, WithRetiredKeys(retired))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body, err := json.Marshal(iss.JWKS())
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}

	var doc struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal JWKS: %v", err)
	}
	if len(doc.Keys) != 2 {
		t.Fatalf("published %d keys, want 2", len(doc.Keys))
	}
	for i, k := range doc.Keys {
		// "d" is the private scalar of an EC key. Its presence is exactly what
		// a leak would look like.
		if _, bad := k["d"]; bad {
			t.Errorf("key %d publishes the private scalar %q", i, "d")
		}
		for _, field := range []string{"p", "q", "dp", "dq", "qi", "k"} {
			if _, bad := k[field]; bad {
				t.Errorf("key %d publishes private field %q", i, field)
			}
		}
		if k["kty"] != "EC" || k["crv"] != "P-256" {
			t.Errorf("key %d = kty %v crv %v, want EC/P-256", i, k["kty"], k["crv"])
		}
	}

	// And the round trip lands on a public key type, not a private one.
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("re-parse JWKS: %v", err)
	}
	for i, k := range set.Keys {
		if _, isPrivate := k.Key.(*ecdsa.PrivateKey); isPrivate {
			t.Errorf("key %d parses back as an ecdsa private key", i)
		}
		if _, isPublic := k.Key.(*ecdsa.PublicKey); !isPublic {
			t.Errorf("key %d = %T, want *ecdsa.PublicKey", i, k.Key)
		}
	}
}

// TestMintRefusesAnIncompleteGrant pins the issuer refusing to sign something
// no console would accept. Without it the failure surfaces at whichever
// console the user was sent to, as a bare 401, with the actual mistake several
// systems away.
func TestMintRefusesAnIncompleteGrant(t *testing.T) {
	iss, err := New("https://cloud.atlantis.dev", mustKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name  string
		mutex func(*Grant)
		want  string
	}{
		{"no subject", func(g *Grant) { g.Subject = "" }, "sub"},
		{"no org", func(g *Grant) { g.Org = "" }, "org"},
		{"no email", func(g *Grant) { g.Email = "" }, "email"},
		{"no role", func(g *Grant) { g.Role = "" }, "role"},
		{"no audience", func(g *Grant) { g.Audience = "" }, "aud"},
		{"unknown role", func(g *Grant) { g.Role = identity.Role("administrator") }, "administrator"},
		{"role differing in case", func(g *Grant) { g.Role = identity.Role("Admin") }, "Admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := validGrant()
			tc.mutex(&g)

			tok, err := iss.Mint(g)
			if err == nil {
				t.Fatalf("minted an assertion for an invalid grant: %s", tok)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	// The counterpart: a complete grant does mint. Without this the table
	// above would pass just as well against a Mint that always failed.
	if _, err := iss.Mint(validGrant()); err != nil {
		t.Fatalf("Mint(valid grant): %v", err)
	}
}

func TestMintRequiresRoleThatConsoleUnderstands(t *testing.T) {
	// identity.Role's whole purpose. Kept separate from the table above
	// because it is the case where the grant is complete and still wrong.
	var c identity.Claims
	if err := (identity.Claims{
		Subject: "u", Org: "o", Email: "e", Role: identity.Role("viewer "), Expiry: time.Now(),
	}).Validate(); err == nil {
		t.Error("a role with trailing whitespace was accepted")
	}
	c = identity.Claims{Subject: "u", Org: "o", Email: "e", Role: identity.RoleViewer, Expiry: time.Now()}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(viewer): %v", err)
	}
}

// TestClaimsValidateRequiresExpiry covers the gap go-jose leaves: its
// validator reads `if c.Expiry != nil`, so a token with no exp claim passes
// every time check it makes. An assertion that never expires is a permanent
// credential.
func TestClaimsValidateRequiresExpiry(t *testing.T) {
	c := identity.Claims{Subject: "u", Org: "o", Email: "e", Role: identity.RoleAdmin}
	err := c.Validate()
	if err == nil {
		t.Fatal("claims with no expiry were accepted")
	}
	if !errors.Is(err, identity.ErrMissingClaim) {
		t.Errorf("err = %v, want ErrMissingClaim", err)
	}
}

func TestNewRequiresANameAndAKey(t *testing.T) {
	if _, err := New("", mustKey(t)); err == nil {
		t.Error("an issuer with no name was accepted; iss would be empty and every console's issuer check is an exact match")
	}
	if _, err := New("https://cloud.atlantis.dev", nil); err == nil {
		t.Error("an issuer with no signing key was accepted")
	}
}

func TestKeyIDsAreDistinctAndStable(t *testing.T) {
	a, b := mustKey(t), mustKey(t)
	if a.ID == b.ID {
		t.Fatalf("two generated keys share a kid: %s", a.ID)
	}
	if a.ID != a.PublicJWK().KeyID {
		t.Errorf("kid %q does not match the published one %q", a.ID, a.PublicJWK().KeyID)
	}
	if a.ID == "" {
		t.Error("kid is empty; the console requires one and would refuse every assertion")
	}
}

func TestHandlerServesTheKeySet(t *testing.T) {
	active, retired := mustKey(t), mustKey(t)
	iss, err := New("https://cloud.atlantis.dev", active, WithRetiredKeys(retired))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(iss.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + JWKSPath)
	if err != nil {
		t.Fatalf("GET %s: %v", JWKSPath, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/jwk-set+json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var set jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Both keys, so an assertion signed just before a rotation still verifies.
	if len(set.Key(active.ID)) != 1 {
		t.Error("active key is not published")
	}
	if len(set.Key(retired.ID)) != 1 {
		t.Error("retired key is not published; assertions still in flight would stop verifying")
	}
}
