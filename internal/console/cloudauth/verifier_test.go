package cloudauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
)

const (
	testIssuerName = "https://cloud.atlantis.dev"
	testAudience   = "https://acme.console.atlantis.dev"
)

var testNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// A cloud we control completely.
//
// The end-to-end test at the bottom uses the real issuer package. Everything
// else needs to mint assertions the real issuer deliberately refuses to
// produce — no expiry, an unknown role, alg none — so it signs by hand.
// ---------------------------------------------------------------------------

type testKey struct {
	priv *ecdsa.PrivateKey
	kid  string
}

func newTestKey(t *testing.T) *testKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	k := &testKey{priv: priv}
	jwk := jose.JSONWebKey{Key: priv.Public(), Algorithm: string(jose.ES256), Use: "sig"}
	tp, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("thumbprint: %v", err)
	}
	k.kid = base64.RawURLEncoding.EncodeToString(tp)
	return k
}

func (k *testKey) publicJWK() jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       k.priv.Public(),
		KeyID:     k.kid,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}
}

// sign serialises claims as a JWS. kid is passed separately so a test can sign
// with one key while naming another.
func (k *testKey) sign(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k.priv, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return tok
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type testCloud struct {
	mu      sync.Mutex
	keys    []*testKey
	down    bool
	fetches int
	srv     *httptest.Server
}

func newTestCloud(t *testing.T, keys ...*testKey) *testCloud {
	t.Helper()
	c := &testCloud{keys: keys}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.fetches++
		down := c.down
		set := jose.JSONWebKeySet{}
		for _, k := range c.keys {
			set.Keys = append(set.Keys, k.publicJWK())
		}
		c.mu.Unlock()

		if down {
			http.Error(w, "cloud is down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *testCloud) url() string { return c.srv.URL + "/.well-known/jwks.json" }

func (c *testCloud) publish(keys ...*testKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = keys
}

func (c *testCloud) setDown(down bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = down
}

func (c *testCloud) fetchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches
}

// validClaims is a complete, correct claim set. Every negative case below
// starts here and changes exactly one thing, so a rejection can only be caused
// by that change.
func validClaims() map[string]any {
	return map[string]any{
		"iss":   testIssuerName,
		"sub":   "usr_01HQ",
		"aud":   []string{testAudience},
		"iat":   testNow.Unix(),
		"nbf":   testNow.Unix(),
		"exp":   testNow.Add(2 * time.Minute).Unix(),
		"org":   "acme",
		"role":  "admin",
		"email": "rachit@example.com",
		"name":  "Rachit Kumar",
	}
}

// claimsAt is validClaims with the time bounds moved to now.
//
// Any test that advances the clock must mint with this rather than
// validClaims, or the assertion expires along the way and the test starts
// passing because the token was stale — not because of the thing it is
// actually about. Two of the tests below were written the wrong way first and
// failed for exactly that reason.
func claimsAt(now time.Time) map[string]any {
	c := validClaims()
	c["iat"] = now.Unix()
	c["nbf"] = now.Unix()
	c["exp"] = now.Add(2 * time.Minute).Unix()
	return c
}

func newVerifier(t *testing.T, c *testCloud, clk *testClock, tune ...func(*Config)) *Verifier {
	t.Helper()
	cfg := Config{
		Issuer:   testIssuerName,
		Audience: testAudience,
		JWKSURL:  c.url(),
		Clock:    clk.now,
	}
	for _, f := range tune {
		f(&cfg)
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------

func TestVerifyAcceptsAValidAssertion(t *testing.T) {
	key := newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk)

	claims, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims()))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if claims.Subject != "usr_01HQ" {
		t.Errorf("Subject = %q", claims.Subject)
	}
	if claims.Org != "acme" {
		t.Errorf("Org = %q", claims.Org)
	}
	if claims.Role != identity.RoleAdmin {
		t.Errorf("Role = %q", claims.Role)
	}
	if claims.Email != "rachit@example.com" {
		t.Errorf("Email = %q", claims.Email)
	}
	if claims.Name != "Rachit Kumar" {
		t.Errorf("Name = %q", claims.Name)
	}
	if want := testNow.Add(2 * time.Minute); !claims.Expiry.Equal(want) {
		t.Errorf("Expiry = %v, want %v", claims.Expiry, want)
	}
}

// TestVerifyRejects is the substance of this package.
//
// Each case takes the valid claim set and breaks one thing. The control
// assertion before the loop is not decoration: without it, a Verify that
// returned an error unconditionally would pass every case below.
func TestVerifyRejects(t *testing.T) {
	key := newTestKey(t)
	other := newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk)

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims())); err != nil {
		t.Fatalf("control assertion did not verify, so the cases below prove nothing: %v", err)
	}

	without := func(field string) map[string]any {
		c := validClaims()
		delete(c, field)
		return c
	}
	with := func(field string, val any) map[string]any {
		c := validClaims()
		c[field] = val
		return c
	}

	for _, tc := range []struct {
		name  string
		token func(t *testing.T) string
		// wantErr, where set, pins WHICH check did the refusing. Without it a
		// case can pass for an unrelated reason — the symmetric-algorithm case
		// below did exactly that, failing on a key-type mismatch rather than
		// on the allow-list it was written to cover.
		wantErr string
	}{
		{name: "issuer is someone else", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("iss", "https://not-cloud.example.com"))
		}},
		{name: "audience is another org's console", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("aud", []string{"https://other.console.atlantis.dev"}))
		}},
		{name: "audience is absent", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("aud"))
		}},
		{name: "expired", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("exp", testNow.Add(-10*time.Minute).Unix()))
		}},
		{name: "not valid yet", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("nbf", testNow.Add(10*time.Minute).Unix()))
		}},
		{name: "issued in the future", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("iat", testNow.Add(10*time.Minute).Unix()))
		}},

		// go-jose's validator reads `if c.Expiry != nil`, so this token
		// passes every time check it makes. identity.Claims.Validate is what
		// refuses it. An assertion with no expiry is a permanent credential.
		{name: "no expiry at all", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("exp"))
		}},

		{name: "signed by a key that is not published", token: func(t *testing.T) string {
			return other.sign(t, other.kid, validClaims())
		}},

		// Sharper than the previous case: the kid names a key the console
		// really does hold, so the lookup succeeds and only the signature
		// check can catch it.
		{name: "signed by another key under a published kid", token: func(t *testing.T) string {
			return other.sign(t, key.kid, validClaims())
		}},

		{name: "no kid in the header", token: func(t *testing.T) string {
			return key.sign(t, "", validClaims())
		}},

		// Both of these must be refused by the algorithm allow-list, at the
		// parser, before any key is looked up — hence the pinned message. A
		// rejection anywhere later would mean the allow-list is not what is
		// doing the work, and the guarantee would evaporate the moment the
		// surrounding code changed.
		{name: "unsigned, alg none", token: func(t *testing.T) string {
			return unsignedToken(t, validClaims())
		}, wantErr: "unexpected signature algorithm"},
		{name: "symmetric alg, the confusion attack", token: func(t *testing.T) string {
			return hmacToken(t, key.kid, validClaims())
		}, wantErr: "unexpected signature algorithm"},

		{name: "no subject", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("sub"))
		}},
		{name: "no org", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("org"))
		}},
		{name: "no email", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("email"))
		}},
		{name: "no role", token: func(t *testing.T) string {
			return key.sign(t, key.kid, without("role"))
		}},
		{name: "role this console does not know", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("role", "administrator"))
		}},
		{name: "role differing only in case", token: func(t *testing.T) string {
			return key.sign(t, key.kid, with("role", "Admin"))
		}},

		{name: "not a token at all", token: func(t *testing.T) string {
			return "this is not a jwt"
		}},
		{name: "empty", token: func(t *testing.T) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := v.Verify(context.Background(), tc.token(t))
			if err == nil {
				t.Fatalf("accepted: %+v", claims)
			}
			if claims != nil {
				t.Errorf("returned claims alongside an error: %+v", claims)
			}
			if !errors.Is(err, ErrUnverified) {
				t.Errorf("err = %v, want it to wrap ErrUnverified so the handler answers 401", err)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q — it was refused, but not by the check this case is about", err, tc.wantErr)
			}
		})
	}
}

// TestAllowedAlgorithmsAreAsymmetricOnly states the property the list exists
// for, rather than trusting that a future edit will reason it out again.
//
// A symmetric entry is not a weakening, it is a total break: HMAC verification
// takes the issuer's key as the shared secret, and that key is published, so
// anybody could mint an assertion that verifies for any user in any
// organisation.
func TestAllowedAlgorithmsAreAsymmetricOnly(t *testing.T) {
	asymmetric := map[jose.SignatureAlgorithm]bool{
		jose.RS256: true, jose.RS384: true, jose.RS512: true,
		jose.ES256: true, jose.ES384: true, jose.ES512: true,
		jose.PS256: true, jose.PS384: true, jose.PS512: true,
		jose.EdDSA: true,
	}
	if len(allowedAlgorithms) == 0 {
		t.Fatal("no algorithms are allowed, so every assertion is refused")
	}
	for _, a := range allowedAlgorithms {
		if !asymmetric[a] {
			t.Errorf("%q is allowed but is not an asymmetric signature algorithm; "+
				"verifying with it would use Cloud's published key as a shared secret", a)
		}
	}
}

// TestTokenWithNoKidIsRefusedEvenWhenAKeyHasNoKid is why the verifier requires
// a kid rather than leaving the matter to the key lookup.
//
// RFC 7517 makes kid optional, and go-jose's Key(kid) matches on equality — so
// a key published without one is returned by a lookup for "". An issuer that
// omitted kid would therefore have every unkeyed token verify against it.
// Cloud always sets kid, but this console must not depend on that holding.
func TestTokenWithNoKidIsRefusedEvenWhenAKeyHasNoKid(t *testing.T) {
	key := newTestKey(t)
	key.kid = "" // published with no kid, which the spec permits
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk)

	if _, err := v.Verify(context.Background(), key.sign(t, "", validClaims())); err == nil {
		t.Fatal("a token with no kid verified against a key published with no kid")
	}
}

// unsignedToken hand-builds an `alg: none` token, which no signing library
// will produce.
func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	hdr := map[string]any{"alg": "none", "typ": "JWT"}
	return b64(hdr) + "." + b64(claims) + "."
}

// hmacToken signs with HS256. This is the shape of the algorithm-confusion
// attack: a verifier that accepts symmetric algorithms would fetch the
// issuer's key by kid and use it as an HMAC secret — and that key is
// published, so anyone could mint a token that verifies. The allow-list
// refuses it at the parser, before any key is consulted.
func hmacToken(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	secret := []byte("public key material would go here in a real attack")
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: jose.JSONWebKey{Key: secret, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new hmac signer: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("hmac sign: %v", err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return tok
}

// TestLeewayIsBounded documents that skew tolerance exists and how far it
// goes. A token a few seconds past expiry is accepted; one past the leeway is
// not. Without the second half, leeway could be widened to an hour and no test
// would notice.
func TestLeewayIsBounded(t *testing.T) {
	key := newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk)

	justExpired := validClaims()
	justExpired["exp"] = testNow.Add(-DefaultLeeway / 2).Unix()
	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, justExpired)); err != nil {
		t.Errorf("a token %v past expiry was refused, so clock skew signs users out: %v", DefaultLeeway/2, err)
	}

	wellExpired := validClaims()
	wellExpired["exp"] = testNow.Add(-2 * DefaultLeeway).Unix()
	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, wellExpired)); err == nil {
		t.Errorf("a token %v past expiry was accepted", 2*DefaultLeeway)
	}
}

// TestRotation covers the promotion of a new signing key. The console has
// never seen the new kid, so the only thing that can save the sign-in is the
// refetch triggered by an unrecognised key.
func TestRotation(t *testing.T) {
	oldKey, newKey := newTestKey(t), newTestKey(t)
	cloud := newTestCloud(t, oldKey)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk, func(c *Config) { c.MinRefetch = time.Nanosecond })

	if _, err := v.Verify(context.Background(), oldKey.sign(t, oldKey.kid, validClaims())); err != nil {
		t.Fatalf("before rotation: %v", err)
	}

	// Cloud rotates, publishing both for the overlap window.
	cloud.publish(newKey, oldKey)
	clk.advance(time.Second)

	if _, err := v.Verify(context.Background(), newKey.sign(t, newKey.kid, claimsAt(clk.now()))); err != nil {
		t.Errorf("assertion from the new key was refused, so every sign-in fails until the cache expires: %v", err)
	}
	// The overlap is the point: an assertion minted moments before the switch
	// is still in flight and must still verify.
	if _, err := v.Verify(context.Background(), oldKey.sign(t, oldKey.kid, claimsAt(clk.now()))); err != nil {
		t.Errorf("assertion from the retired key was refused during the overlap window: %v", err)
	}

	// Withdrawal. A key Cloud has stopped publishing keeps verifying here
	// until the cached set is refreshed — this is the exposure window after a
	// key compromise, and it is bounded by RefreshAfter and nothing else.
	cloud.publish(newKey)
	clk.advance(time.Second)
	if _, err := v.Verify(context.Background(), oldKey.sign(t, oldKey.kid, claimsAt(clk.now()))); err != nil {
		t.Errorf("within the refresh window a withdrawn key should still verify; "+
			"if this now fails the cache invalidates eagerly and an outage would sign everyone out: %v", err)
	}

	clk.advance(defaultRefreshAfter + time.Second)
	if _, err := v.Verify(context.Background(), oldKey.sign(t, oldKey.kid, claimsAt(clk.now()))); err == nil {
		t.Errorf("a withdrawn key still verifies more than %v after withdrawal", defaultRefreshAfter)
	}
}

// TestCloudOutageDoesNotSignEveryoneOut is the availability half. Cloud is
// unreachable; assertions already in hand must keep working.
func TestCloudOutageDoesNotSignEveryoneOut(t *testing.T) {
	key := newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk, func(c *Config) { c.RefreshAfter = time.Minute })

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims())); err != nil {
		t.Fatalf("while cloud is up: %v", err)
	}

	cloud.setDown(true)
	clk.advance(10 * time.Minute) // well past RefreshAfter, so a refresh is attempted and fails

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, claimsAt(clk.now()))); err != nil {
		t.Errorf("a failed key refresh signed the user out: %v", err)
	}
	if cloud.fetchCount() < 2 {
		t.Error("no refresh was attempted, so this passed without exercising the failure")
	}
}

// TestNoKeysAndCloudDownIsUnavailable separates "cannot check" from "checked
// and it was bad". The console owes a 503 here, not a 401: nothing is known to
// be wrong with the credential.
func TestNoKeysAndCloudDownIsUnavailable(t *testing.T) {
	key := newTestKey(t)
	cloud := newTestCloud(t, key)
	cloud.setDown(true)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk)

	_, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims()))
	if err == nil {
		t.Fatal("verified an assertion with no keys at all")
	}
	if !errors.Is(err, ErrKeysUnavailable) {
		t.Errorf("err = %v, want ErrKeysUnavailable", err)
	}
	if errors.Is(err, ErrUnverified) {
		t.Error("reported as a bad assertion; the handler would answer 401 for a cloud outage")
	}
}

// TestUnknownKidRefetchIsRateLimited stops an unauthenticated caller aiming
// this console at Cloud: a stream of tokens naming a kid that does not exist
// would otherwise become one outbound fetch each.
func TestUnknownKidRefetchIsRateLimited(t *testing.T) {
	key, stranger := newTestKey(t), newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk, func(c *Config) { c.MinRefetch = time.Minute })

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims())); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}
	after := cloud.fetchCount()

	for range 20 {
		if _, err := v.Verify(context.Background(), stranger.sign(t, stranger.kid, validClaims())); err == nil {
			t.Fatal("a token from an unpublished key verified")
		}
	}
	if got := cloud.fetchCount(); got != after {
		t.Errorf("20 forged tokens caused %d fetches; want none within MinRefetch", got-after)
	}

	// Past the window, one more attempt is allowed — that is what makes
	// rotation prompt.
	clk.advance(2 * time.Minute)
	if _, err := v.Verify(context.Background(), stranger.sign(t, stranger.kid, validClaims())); err == nil {
		t.Fatal("a token from an unpublished key verified")
	}
	if got := cloud.fetchCount(); got != after+1 {
		t.Errorf("fetches after the window = %d, want %d", got, after+1)
	}
}

// TestKeySetIsNotReplacedByAnEmptyOne covers a JWKS endpoint that answers 200
// with no keys — a plausible shape for a misconfigured deployment. Taking it
// at face value would discard working keys and report the failure as a bad
// token.
func TestKeySetIsNotReplacedByAnEmptyOne(t *testing.T) {
	key := newTestKey(t)
	cloud := newTestCloud(t, key)
	clk := &testClock{t: testNow}
	v := newVerifier(t, cloud, clk, func(c *Config) { c.RefreshAfter = time.Minute })

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, validClaims())); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	cloud.publish() // 200, {"keys":[]}
	clk.advance(10 * time.Minute)

	if _, err := v.Verify(context.Background(), key.sign(t, key.kid, claimsAt(clk.now()))); err != nil {
		t.Errorf("an empty key set replaced a working one: %v", err)
	}
}

func TestNewRequiresIssuerAudienceAndJWKSURL(t *testing.T) {
	// go-jose skips the issuer check when the expected value is empty, and
	// skips the audience check when the expected set is empty. A Verifier
	// built from a partial Config would run and accept assertions from any
	// issuer for any console, so refusing to build one is the only reliable
	// place to catch it.
	base := Config{Issuer: testIssuerName, Audience: testAudience, JWKSURL: "https://cloud.atlantis.dev/jwks"}

	for _, tc := range []struct {
		name string
		drop func(*Config)
		want string
	}{
		{"no issuer", func(c *Config) { c.Issuer = "" }, "issuer"},
		{"no audience", func(c *Config) { c.Audience = "" }, "audience"},
		{"no JWKS URL", func(c *Config) { c.JWKSURL = "" }, "JWKS URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.drop(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatal("built a verifier that would accept anything")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}

	if _, err := New(base); err != nil {
		t.Fatalf("New(complete config): %v", err)
	}
}

// TestEndToEndWithTheRealIssuer runs the two halves against each other: the
// issuer package mints, its own handler publishes the keys, and the verifier
// checks the result. Nothing here is a mock, so a change to the claim shape on
// either side fails this test rather than passing on both.
func TestEndToEndWithTheRealIssuer(t *testing.T) {
	key, err := issuer.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	iss, err := issuer.New(testIssuerName, key)
	if err != nil {
		t.Fatalf("issuer.New: %v", err)
	}
	srv := httptest.NewServer(iss.Handler())
	t.Cleanup(srv.Close)

	v, err := New(Config{
		Issuer:   iss.Name(),
		Audience: testAudience,
		JWKSURL:  srv.URL + issuer.JWKSPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, err := iss.Mint(issuer.Grant{
		Subject:  "usr_01HQ",
		Org:      "acme",
		Role:     identity.RoleViewer,
		Email:    "rachit@example.com",
		Name:     "Rachit Kumar",
		Audience: testAudience,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	claims, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "usr_01HQ" || claims.Org != "acme" || claims.Role != identity.RoleViewer {
		t.Errorf("claims = %+v", claims)
	}
	if claims.Email != "rachit@example.com" || claims.Name != "Rachit Kumar" {
		t.Errorf("claims = %+v", claims)
	}

	// A second console, same issuer, different audience: the assertion must
	// not travel. One stack per org means this is the boundary between
	// organisations.
	otherConsole, err := New(Config{
		Issuer:   iss.Name(),
		Audience: "https://other.console.atlantis.dev",
		JWKSURL:  srv.URL + issuer.JWKSPath,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := otherConsole.Verify(context.Background(), token); err == nil {
		t.Error("an assertion for one organisation's console verified at another's")
	}
}
