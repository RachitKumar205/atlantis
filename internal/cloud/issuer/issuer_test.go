package issuer

import (
	"crypto/ecdsa"
	"encoding/base64"
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

// TestEveryAssertionGetsADistinctID pins what single-use enforcement rests on.
// If Mint reused an id, the second sign-in of a session would be refused as a
// replay of the first — and the failure would look like a broken console
// rather than like the collision it is.
func TestEveryAssertionGetsADistinctID(t *testing.T) {
	iss, err := New("https://cloud.atlantis.dev", mustKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seen := make(map[string]bool)
	for range 50 {
		tok, err := iss.Mint(validGrant())
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		id := assertionID(t, tok)
		if id == "" {
			t.Fatal("minted an assertion with no jti; the console requires one")
		}
		if seen[id] {
			t.Fatalf("assertion id %q was minted twice", id)
		}
		seen[id] = true
	}
}

// assertionID reads jti out of a token's payload without verifying it. Only
// safe because the test minted the token itself.
func assertionID(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		ID string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return claims.ID
}

// completeClaims is a claim set that passes Validate. Each test below removes
// or spoils exactly one field, so a rejection can only be caused by that field
// — otherwise a test asserting "no expiry is refused" passes on a set that is
// also missing three other things.
func completeClaims() identity.Claims {
	return identity.Claims{
		ID:      "assertion_1",
		Subject: "usr_1",
		Org:     "acme",
		Email:   "rachit@example.com",
		Role:    identity.RoleAdmin,
		Expiry:  time.Now().Add(time.Minute),
	}
}

func TestMintRequiresRoleThatConsoleUnderstands(t *testing.T) {
	// identity.Role's whole purpose. Kept separate from the table above
	// because it is the case where the claim set is complete and still wrong.
	if err := completeClaims().Validate(); err != nil {
		t.Fatalf("the control claim set does not validate, so the cases below prove nothing: %v", err)
	}

	spoiled := completeClaims()
	spoiled.Role = identity.Role("viewer ")
	if err := spoiled.Validate(); err == nil {
		t.Error("a role with trailing whitespace was accepted")
	}

	ok := completeClaims()
	ok.Role = identity.RoleViewer
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(viewer): %v", err)
	}
}

// TestClaimsValidateRequiresExpiry covers the gap go-jose leaves: its
// validator reads `if c.Expiry != nil`, so a token with no exp claim passes
// every time check it makes. An assertion that never expires is a permanent
// credential.
func TestClaimsValidateRequiresExpiry(t *testing.T) {
	c := completeClaims()
	c.Expiry = time.Time{}

	err := c.Validate()
	if err == nil {
		t.Fatal("claims with no expiry were accepted")
	}
	if !errors.Is(err, identity.ErrMissingClaim) {
		t.Errorf("err = %v, want ErrMissingClaim", err)
	}
	if !strings.Contains(err.Error(), "exp") {
		t.Errorf("err = %v, want it to name exp — it was refused, but not for the missing expiry", err)
	}
}

// TestClaimsValidateRequiresAnAssertionID pins the other half: without jti the
// console has nothing to record, so refusing a replayed assertion becomes
// impossible.
func TestClaimsValidateRequiresAnAssertionID(t *testing.T) {
	c := completeClaims()
	c.ID = ""

	err := c.Validate()
	if err == nil {
		t.Fatal("claims with no assertion id were accepted")
	}
	if !strings.Contains(err.Error(), "jti") {
		t.Errorf("err = %v, want it to name jti", err)
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
