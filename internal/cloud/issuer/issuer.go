// Package issuer mints the signed assertions a console exchanges for a
// session, and publishes the public keys those assertions are verified
// against.
//
// A console verifies against a JWKS URL in every environment, with no
// development bypass. Running this on a loopback port during development is the
// same code at a different address.
package issuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// JWKSPath is where Handler publishes the key set. The console is configured
// with a full URL, so this is Cloud's own layout rather than a contract. It
// follows RFC 8615.
const JWKSPath = "/.well-known/jwks.json"

// SigningAlgorithm is the only algorithm this issuer signs with.
//
// ES256 over RS256 for size: an assertion is posted by a browser, and a P-256
// signature is 64 bytes against RSA-2048's 256. The verifier accepts both, so
// this can change without a flag day.
const SigningAlgorithm = jose.ES256

// DefaultTTL is how long a minted assertion is valid for.
//
// It is short because the assertion's only job is to be exchanged, once, for a
// session cookie — the session carries its own, much longer, sliding lifetime.
// A window of minutes covers a slow browser redirect and clock skew between
// Cloud and a console; anything longer just widens the replay window on a
// credential that is handled by JavaScript.
const DefaultTTL = 2 * time.Minute

// Key is a signing key and the `kid` that identifies it in the published key
// set.
type Key struct {
	// ID is the RFC 7638 thumbprint of the public key, base64url encoded.
	//
	// Derived from the key, so two keys cannot collide on a kid and a key's
	// identity survives being reloaded from storage. An unfamiliar kid is a
	// different key, not a renamed one.
	ID string

	priv *ecdsa.PrivateKey
}

// GenerateKey creates a P-256 signing key.
func GenerateKey() (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	k := &Key{priv: priv}
	id, err := thumbprintOf(k)
	if err != nil {
		return nil, err
	}
	k.ID = id
	return k, nil
}

// thumbprintOf computes a key's RFC 7638 thumbprint, which becomes its kid.
//
// Shared with the key-file loader, so a key reloaded from disk keeps the kid it
// was generated with. Two implementations would republish the same key under a
// new name across a restart, orphaning every assertion in flight.
func thumbprintOf(k *Key) (string, error) {
	pub := k.PublicJWK()
	tp, err := pub.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("thumbprint signing key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(tp), nil
}

// PublicJWK returns the public half of the key, in the form it is published.
//
// Built from priv.Public(), so the returned value does not hold the private
// key at all. A test asserts the served JWKS carries no "d" parameter.
func (k *Key) PublicJWK() jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       k.priv.Public(),
		KeyID:     k.ID,
		Algorithm: string(SigningAlgorithm),
		Use:       "sig",
	}
}

// Issuer mints assertions and publishes the keys that verify them.
type Issuer struct {
	name    string
	active  *Key
	retired []*Key
}

// Option configures an Issuer.
type Option func(*Issuer)

// WithRetiredKeys publishes keys that are no longer used for signing.
//
// A key stays published for as long as assertions signed by it may be in
// flight, which is DefaultTTL, so promoting a new signing key never leaves a
// console holding a token whose kid the key set no longer lists.
func WithRetiredKeys(keys ...*Key) Option {
	return func(i *Issuer) { i.retired = append(i.retired, keys...) }
}

// New returns an Issuer that signs with active and identifies itself as name.
//
// name becomes the `iss` claim, and each console is configured with the value
// it will accept, so the two must match exactly. It should be the issuer's
// https URL.
func New(name string, active *Key, opts ...Option) (*Issuer, error) {
	if name == "" {
		return nil, fmt.Errorf("issuer name is required: it becomes the iss claim")
	}
	if active == nil {
		return nil, fmt.Errorf("issuer %s: an active signing key is required", name)
	}
	i := &Issuer{name: name, active: active}
	for _, o := range opts {
		o(i)
	}
	return i, nil
}

// Name is the value this issuer puts in `iss`.
func (i *Issuer) Name() string { return i.name }

// JWKS is the published key set: the active key first, then any retired keys
// still inside their overlap window.
func (i *Issuer) JWKS() jose.JSONWebKeySet {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{i.active.PublicJWK()}}
	for _, k := range i.retired {
		set.Keys = append(set.Keys, k.PublicJWK())
	}
	return set
}

// Grant is the identity Cloud is asserting, and the console it is asserting it
// to.
type Grant struct {
	// Subject is Cloud's stable user id. It becomes the audit actor.
	Subject string

	// Org is the organisation the user is acting in.
	Org string

	Role  identity.Role
	Email string
	Name  string

	// Audience names the console this assertion is for, so one minted for a
	// given organisation's console cannot be replayed against another's.
	//
	// It is the organisation's registered console URL, which is also where the
	// browser is sent, so audience and destination cannot disagree.
	Audience string

	// StepUp says a second factor was presented for this assertion. Set only by
	// the reauth path; see identity.Claims.StepUp for what rests on it.
	StepUp bool

	// Orgs names every organisation the subject belongs to, for a console's
	// organisation switcher. A hint only — see identity.Claims.Orgs.
	Orgs []string

	// OrgNames maps an organisation's name to its display name, for the ones
	// that have a different one. A hint, like Orgs.
	OrgNames map[string]string
}

// Mint returns a signed assertion for g.
//
// The claims run through the same identity.Claims.Validate the verifier uses,
// so an incomplete grant fails here with the field named rather than as a
// rejection at whichever console the browser reached.
func (i *Issuer) Mint(g Grant) (string, error) {
	if g.Audience == "" {
		return "", fmt.Errorf("%w: aud", identity.ErrMissingClaim)
	}

	now := time.Now().UTC()
	expiry := now.Add(DefaultTTL)

	jti, err := newTokenID()
	if err != nil {
		return "", err
	}

	claims := identity.Claims{
		ID:       jti,
		Subject:  g.Subject,
		Org:      g.Org,
		Role:     g.Role,
		Email:    g.Email,
		Name:     g.Name,
		Expiry:   expiry,
		StepUp:   g.StepUp,
		Orgs:     g.Orgs,
		OrgNames: g.OrgNames,
	}
	if err := claims.Validate(); err != nil {
		return "", fmt.Errorf("refusing to mint an assertion no console would accept: %w", err)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: SigningAlgorithm, Key: jose.JSONWebKey{
			Key:   i.active.priv,
			KeyID: i.active.ID,
		}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		return "", fmt.Errorf("build signer: %w", err)
	}

	registered := jwt.Claims{
		ID:        jti,
		Issuer:    i.name,
		Subject:   g.Subject,
		Audience:  jwt.Audience{g.Audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		Expiry:    jwt.NewNumericDate(expiry),
	}
	private := identity.Private{
		Org:      g.Org,
		Role:     g.Role,
		Email:    g.Email,
		Name:     g.Name,
		StepUp:   g.StepUp,
		Orgs:     g.Orgs,
		OrgNames: g.OrgNames,
	}

	tok, err := jwt.Signed(signer).Claims(registered).Claims(private).Serialize()
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}
	return tok, nil
}

// newTokenID returns the `jti` for one assertion: 128 bits from crypto/rand.
//
// Unpredictable, not merely unique. A console records spent assertion ids to
// refuse replay, so a guessable jti can be burned before the assertion carrying
// it arrives.
func newTokenID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate assertion id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Handler serves the public key set at JWKSPath.
func (i *Issuer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+JWKSPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(i.JWKS())
		if err != nil {
			http.Error(w, "could not encode key set", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")

		// Consoles cache on their own schedule and refetch on an unrecognised
		// kid, so this bounds intermediaries only. Under the key overlap
		// window, so a proxy cannot serve a pre-rotation set for longer than
		// the retired key stays valid.
		w.Header().Set("Cache-Control", "public, max-age=300")

		_, _ = w.Write(body)
	})
	return mux
}
