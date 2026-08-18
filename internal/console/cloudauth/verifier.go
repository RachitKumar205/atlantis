// Package cloudauth verifies the signed assertions Atlantis Cloud issues, and
// is the console's only source of identity.
//
// The console holds no credentials of its own: there are no local accounts, no
// password hashes and no user table. A browser arrives with an assertion from
// Cloud, this package checks it, and the console mints a session cookie on the
// strength of it. Everything after that — role checks, sudo, audit — reads the
// session, exactly as before.
//
// The shape on the wire is a plain JWT verified against a JWKS endpoint. That
// is deliberate rather than incidental: both halves of this exchange are
// written in this repository, which makes it easy to drift into a private
// format that happens to work, and a private format is one nobody can inspect
// with an ordinary tool or reason about from a specification.
package cloudauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// ErrUnverified reports an assertion this console will not accept: a bad
// signature, a claim that does not match, an expired token. It is the caller's
// signal to answer 401.
//
// It carries no detail about which check failed, on purpose — the wrapped
// error is for the console's own log, not for the response body. Reporting
// "expired" separately from "wrong audience" tells whoever is holding a token
// exactly which part to work on next.
var ErrUnverified = errors.New("assertion not verified")

// ErrKeysUnavailable reports that this console could not obtain Cloud's
// published keys and has none cached, so it cannot tell a good assertion from
// a bad one. That is a 503 and not a 401: nothing is known to be wrong with
// the credential.
var ErrKeysUnavailable = errors.New("cloud signing keys unavailable")

// allowedAlgorithms is the complete set of signature algorithms this console
// will consider, and it is passed to the parser before any key is looked at.
//
// The list is the defence against the two classic JWT forgeries, and it works
// by construction rather than by a check that could be moved or removed.
// `alg: none` is not on it, so an unsigned token is refused before its claims
// are read. No symmetric algorithm is on it either, which is what stops the
// confusion attack: given HS256, a verifier looks up the issuer's key by kid
// and uses it as an HMAC secret — and that key is public, so anyone can mint a
// token that verifies. Restricting the set to asymmetric algorithms means the
// key material required to produce an acceptable signature is material Cloud
// alone holds.
var allowedAlgorithms = []jose.SignatureAlgorithm{jose.ES256, jose.RS256}

// Config describes the issuer this console trusts.
type Config struct {
	// Issuer is the exact `iss` value to require.
	Issuer string

	// Audience is the exact value to require in `aud`. It names this console,
	// and it is what stops an assertion minted for one organisation's console
	// being replayed against another's — every console runs the same code and
	// trusts the same issuer, so without it a token would be accepted by all
	// of them.
	Audience string

	// JWKSURL is where the issuer publishes its public keys.
	JWKSURL string

	// Leeway absorbs clock skew between Cloud and this console when checking
	// exp, nbf and iat. Zero selects DefaultLeeway.
	//
	// Set explicitly, because go-jose's Validate would otherwise apply a
	// one-minute default that is not stated at the call site.
	Leeway time.Duration

	// RefreshAfter, MinRefetch and HTTPClient tune key fetching. Zero values
	// select the defaults in keycache.go.
	RefreshAfter time.Duration
	MinRefetch   time.Duration
	HTTPClient   *http.Client

	// Clock replaces the time source, for tests.
	Clock func() time.Time
}

// DefaultLeeway is the tolerance applied to the time-based claims.
//
// Thirty seconds: enough for ordinary NTP drift between two hosts, and short
// relative to the assertion's own lifetime, so it does not meaningfully extend
// the replay window.
const DefaultLeeway = 30 * time.Second

// Verifier checks assertions against a single issuer.
type Verifier struct {
	issuer   string
	audience string
	leeway   time.Duration
	now      func() time.Time
	keys     *keyCache
}

// New returns a Verifier for cfg.
//
// Issuer, Audience and JWKSURL are all required and none has a default. That
// is worth being strict about: go-jose skips the issuer check when the
// expected value is empty, and skips the audience check when the expected set
// is empty. A Verifier built from a partially-populated Config would therefore
// not fail — it would run, and quietly accept assertions from any issuer, for
// any console. Refusing to construct one is the only place that can be caught
// reliably.
func New(cfg Config) (*Verifier, error) {
	var missing []string
	if cfg.Issuer == "" {
		missing = append(missing, "issuer")
	}
	if cfg.Audience == "" {
		missing = append(missing, "audience")
	}
	if cfg.JWKSURL == "" {
		missing = append(missing, "JWKS URL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cloudauth: %s must be configured", joinWords(missing))
	}

	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	leeway := cfg.Leeway
	if leeway == 0 {
		leeway = DefaultLeeway
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	refreshAfter := cfg.RefreshAfter
	if refreshAfter == 0 {
		refreshAfter = defaultRefreshAfter
	}
	minRefetch := cfg.MinRefetch
	if minRefetch == 0 {
		minRefetch = defaultMinRefetch
	}

	return &Verifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		leeway:   leeway,
		now:      now,
		keys: &keyCache{
			url:          cfg.JWKSURL,
			client:       client,
			now:          now,
			refreshAfter: refreshAfter,
			minRefetch:   minRefetch,
		},
	}, nil
}

// Verify checks an assertion and returns the identity it asserts.
//
// A non-nil result means all of: the token was signed with an allowed
// asymmetric algorithm, by a key the configured issuer currently publishes;
// `iss` and `aud` match exactly; the token is inside its time bounds; and
// every claim the console needs is present and recognised.
func (v *Verifier) Verify(ctx context.Context, token string) (*identity.Claims, error) {
	parsed, err := jwt.ParseSigned(token, allowedAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnverified, err)
	}

	// Exactly one signature. A JWS may carry several, and accepting a
	// multi-signature token would mean deciding which one authorises the
	// request — a choice with no good answer and an obvious wrong one.
	if len(parsed.Headers) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one signature, got %d", ErrUnverified, len(parsed.Headers))
	}
	hdr := parsed.Headers[0]
	if hdr.KeyID == "" {
		// Cloud always sets kid. Requiring it avoids the alternative, which is
		// to try every published key in turn and so keep verifying against a
		// key the issuer has stopped using for anything.
		return nil, fmt.Errorf("%w: no kid in header", ErrUnverified)
	}

	key, err := v.keys.keyFor(ctx, hdr.KeyID, hdr.Algorithm)
	if err != nil {
		return nil, err
	}

	var (
		registered jwt.Claims
		private    identity.Private
	)
	// Claims verifies the signature before it unmarshals anything, so nothing
	// below this line has read an unauthenticated value.
	if err := parsed.Claims(key, &registered, &private); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnverified, err)
	}

	expected := jwt.Expected{
		Issuer:      v.issuer,
		AnyAudience: jwt.Audience{v.audience},
		Time:        v.now(),
	}
	if err := registered.ValidateWithLeeway(expected, v.leeway); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnverified, err)
	}

	claims := identity.Claims{
		ID:      registered.ID,
		Subject: registered.Subject,
		Org:     private.Org,
		Role:    private.Role,
		Email:   private.Email,
		Name:    private.Name,
	}
	if registered.Expiry != nil {
		claims.Expiry = registered.Expiry.Time()
	}

	// Catches what the signature and the time bounds cannot: a genuine
	// assertion that is missing something. Most importantly a missing `exp` —
	// ValidateWithLeeway above reads `if c.Expiry != nil`, so a token that
	// simply omits the claim passes every time check it makes.
	if err := claims.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnverified, err)
	}
	return &claims, nil
}

func joinWords(w []string) string {
	switch len(w) {
	case 1:
		return w[0]
	case 2:
		return w[0] + " and " + w[1]
	default:
		return w[0] + ", " + joinWords(w[1:])
	}
}
