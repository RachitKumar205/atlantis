// Package oidcfed verifies a CI workload's OIDC id_token against a
// federation rule.
//
// cloudauth verifies Cloud's own assertions and cannot serve here: its claim
// mapping requires org, role and email, which no CI issuer's token carries,
// and its issuer is fixed at construction while rules name arbitrary ones.
// What this package shares with it is the discipline — pinned algorithms, an
// exact issuer and audience, and keys fetched from the issuer's published
// set.
package oidcfed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// allowedAlgorithms pins what a token may be signed with. GitHub signs RS256;
// ES256 covers issuers with EC keys. Everything else — HMAC above all — is
// excluded by construction.
var allowedAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// leeway absorbs clock skew between the issuer and this process.
const leeway = 30 * time.Second

// discoveryTimeout bounds one fetch of a discovery document or key set.
const discoveryTimeout = 10 * time.Second

// keysTTL is how long a fetched key set is served before re-fetching.
const keysTTL = 5 * time.Minute

// maxIssuers bounds the cache. Rules name a handful of issuers; a map that
// grew per verification would grow per attacker.
const maxIssuers = 64

// Verifier verifies tokens for any rule's issuer, caching each issuer's
// discovery and keys.
type Verifier struct {
	client *http.Client

	mu      sync.Mutex
	issuers map[string]*issuerKeys
}

type issuerKeys struct {
	jwksURI   string
	keys      *jose.JSONWebKeySet
	fetchedAt time.Time
}

func New() *Verifier {
	return NewWithClient(&http.Client{Timeout: discoveryTimeout})
}

// NewWithClient exists for tests, whose fake issuer serves a certificate no
// system root signed.
func NewWithClient(client *http.Client) *Verifier {
	return &Verifier{
		client:  client,
		issuers: make(map[string]*issuerKeys),
	}
}

// Identity is what a verified token asserts.
type Identity struct {
	Issuer  string
	Subject string
}

// Verify checks token against the rule's issuer, audience and subject
// pattern, and returns the identity it asserts.
func (v *Verifier) Verify(ctx context.Context, token, issuerURL, audience, subjectPattern string) (*Identity, error) {
	parsed, err := jwt.ParseSigned(token, allowedAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	if len(parsed.Headers) != 1 {
		return nil, fmt.Errorf("token carries %d signatures", len(parsed.Headers))
	}
	h := parsed.Headers[0]

	key, err := v.keyFor(ctx, issuerURL, h.KeyID, h.Algorithm)
	if err != nil {
		return nil, err
	}

	var claims jwt.Claims
	if err := parsed.Claims(key, &claims); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}

	// go-jose skips checks whose expected value is empty, so every field is
	// asserted non-empty before the comparison is trusted.
	if issuerURL == "" || audience == "" {
		return nil, fmt.Errorf("the rule is incomplete")
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      issuerURL,
		AnyAudience: jwt.Audience{audience},
		Time:        time.Now(),
	}, leeway); err != nil {
		return nil, fmt.Errorf("token rejected: %w", err)
	}
	if claims.Expiry == nil {
		return nil, fmt.Errorf("token has no expiry")
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("token has no subject")
	}
	if !MatchSubject(subjectPattern, claims.Subject) {
		return nil, fmt.Errorf("subject %q does not match the rule", claims.Subject)
	}
	return &Identity{Issuer: claims.Issuer, Subject: claims.Subject}, nil
}

// keyFor returns the issuer's key with the given id, fetching or refreshing
// the published set as needed.
func (v *Verifier) keyFor(ctx context.Context, issuerURL, kid string, alg string) (jose.JSONWebKey, error) {
	v.mu.Lock()
	entry, ok := v.issuers[issuerURL]
	if !ok {
		if len(v.issuers) >= maxIssuers {
			v.mu.Unlock()
			return jose.JSONWebKey{}, fmt.Errorf("too many issuers cached")
		}
		entry = &issuerKeys{}
		v.issuers[issuerURL] = entry
	}
	stale := entry.keys == nil || time.Since(entry.fetchedAt) > keysTTL
	v.mu.Unlock()

	if stale {
		if err := v.refresh(ctx, issuerURL, entry); err != nil {
			return jose.JSONWebKey{}, err
		}
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	for _, k := range entry.keys.Keys {
		if k.KeyID == kid && (k.Algorithm == "" || k.Algorithm == alg) {
			return k, nil
		}
	}
	return jose.JSONWebKey{}, fmt.Errorf("no key %q at %s", kid, issuerURL)
}

// refresh walks discovery and fetches the key set.
//
// The discovery document's own issuer must equal the URL it was fetched
// from — RFC 8414's rule, and what stops a rule pointing at a page that
// claims to be someone else's issuer.
func (v *Verifier) refresh(ctx context.Context, issuerURL string, entry *issuerKeys) error {
	if !strings.HasPrefix(issuerURL, "https://") {
		return fmt.Errorf("issuer must be https")
	}

	v.mu.Lock()
	jwksURI := entry.jwksURI
	v.mu.Unlock()

	if jwksURI == "" {
		disco := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, disco, &doc); err != nil {
			return fmt.Errorf("discovery at %s: %w", disco, err)
		}
		if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(issuerURL, "/") {
			return fmt.Errorf("discovery names issuer %q, the rule names %q", doc.Issuer, issuerURL)
		}
		if !strings.HasPrefix(doc.JWKSURI, "https://") {
			return fmt.Errorf("jwks_uri %q is not https", doc.JWKSURI)
		}
		jwksURI = doc.JWKSURI
	}

	var keys jose.JSONWebKeySet
	if err := v.getJSON(ctx, jwksURI, &keys); err != nil {
		return fmt.Errorf("keys at %s: %w", jwksURI, err)
	}

	v.mu.Lock()
	entry.jwksURI = jwksURI
	entry.keys = &keys
	entry.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

func (v *Verifier) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// MatchSubject reports whether sub matches pattern, where '*' matches any
// run of characters including ':'.
//
// Crossing ':' is required: the universal GitHub pattern is
// "repo:owner/name:*", and a '*' that stopped at the next ':' could never
// write it.
func MatchSubject(pattern, sub string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(sub, parts[0]) {
		return false
	}
	sub = sub[len(parts[0]):]
	for i, part := range parts[1:] {
		if i == len(parts)-2 {
			return strings.HasSuffix(sub, part)
		}
		idx := strings.Index(sub, part)
		if idx < 0 {
			return false
		}
		sub = sub[idx+len(part):]
	}
	return len(parts) == 1 && sub == ""
}

// ValidatePattern refuses patterns whose match is too broad to be a rule.
//
// The literal prefix before the first '*' must contain ':' with text on both
// sides, so "repo:acme/api:*" is a rule and "*", "*:*", "acme*" and "repo:*"
// are not. A subject grammar opens with a type segment and then a principal;
// a pattern that pins only the type — "repo:*" — matches the issuer's whole
// population, which on a public issuer is everybody.
func ValidatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("the pattern is empty")
	}
	prefix := pattern
	if i := strings.IndexByte(pattern, '*'); i >= 0 {
		prefix = pattern[:i]
	}
	if prefix == "" {
		return fmt.Errorf("the pattern must not begin with a wildcard")
	}
	colon := strings.IndexByte(prefix, ':')
	if colon <= 0 || colon == len(prefix)-1 {
		return fmt.Errorf("the pattern must pin a principal before the first wildcard, " +
			"like repo:acme/api:*")
	}
	return nil
}
