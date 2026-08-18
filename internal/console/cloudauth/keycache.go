package cloudauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Defaults for the key cache. All three are tuned around the fact that the
// thing being fetched changes on the order of months, while the thing being
// verified lives for minutes.
const (
	// defaultRefreshAfter is how long a fetched key set is served before the
	// next verification triggers a refresh attempt. Elapsing does not
	// invalidate the set — it only makes the next lookup try to update it.
	//
	// It is also, and more importantly, the window during which a key Cloud
	// has *withdrawn* still verifies here. Withdrawal is how a compromised
	// signing key is answered, so this bounds how long a console keeps
	// honouring one. Five minutes matches the Cache-Control the issuer serves,
	// which keeps consoles and any intermediary on the same schedule, and the
	// cost of being wrong in the cheap direction is one small GET per console
	// per five minutes.
	defaultRefreshAfter = 5 * time.Minute

	// defaultMinRefetch bounds how often an unrecognised kid may force a
	// fetch. Without it, a stream of tokens bearing a kid that does not exist
	// — which is what a forged token looks like — would turn every request
	// into an outbound HTTP call against Cloud, letting an unauthenticated
	// caller aim this console at it.
	defaultMinRefetch = 1 * time.Minute

	// defaultFetchTimeout bounds a single JWKS request. It matters more than
	// it looks: keyFor holds the cache lock across the fetch, so this is also
	// the longest one unresponsive endpoint can stall concurrent sign-ins.
	defaultFetchTimeout = 10 * time.Second

	// maxJWKSBytes caps the response body. A key set is a few hundred bytes;
	// the limit exists so that whatever answers on the JWKS URL cannot exhaust
	// the console's memory by streaming indefinitely.
	maxJWKSBytes = 1 << 20
)

// keyCache holds the issuer's published key set and decides when to go and
// fetch it again.
//
// The policy has two halves, and they answer different failures:
//
//   - Refresh on a timer, and additionally whenever a token names a key we do
//     not hold. The second half is what makes rotation work promptly. Waiting
//     for the timer would mean that in the window between Cloud promoting a
//     new signing key and this console's next scheduled refresh, every sign-in
//     fails — for up to refreshAfter, with nothing wrong on either side.
//
//   - Never discard a set because a fetch failed. Cloud being unreachable must
//     not sign anyone out. Note what this trades away: a key withdrawn during
//     an outage stays trusted here until connectivity returns. That is the
//     right side to err on, because the assertion is not the durable
//     credential — it is valid for minutes and is spent immediately on a
//     session. Revoking access means revoking the session, which is a database
//     row this console owns and can delete without reaching Cloud at all.
type keyCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	refreshAfter time.Duration
	minRefetch   time.Duration

	mu          sync.Mutex
	set         *jose.JSONWebKeySet
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error
}

// keyFor returns the published key with the given id, suitable for verifying a
// signature made with alg.
func (c *keyCache) keyFor(ctx context.Context, kid, alg string) (jose.JSONWebKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.set == nil || c.now().Sub(c.fetchedAt) >= c.refreshAfter {
		c.refreshLocked(ctx)
	}
	if k, ok := selectKey(c.set, kid, alg); ok {
		return k, nil
	}

	// Unrecognised. Either the issuer rotated since the last fetch, or the
	// token is forged. Both look identical from here, so refetch — bounded by
	// minRefetch so the forged case cannot be used to generate traffic.
	if c.now().Sub(c.lastAttempt) >= c.minRefetch {
		c.refreshLocked(ctx)
		if k, ok := selectKey(c.set, kid, alg); ok {
			return k, nil
		}
	}

	if c.set == nil {
		return jose.JSONWebKey{}, fmt.Errorf("%w: %v", ErrKeysUnavailable, c.lastErr)
	}
	return jose.JSONWebKey{}, fmt.Errorf("%w: no published key %q for algorithm %s", ErrUnverified, kid, alg)
}

// refreshLocked attempts to update the cached set, leaving the existing one in
// place if the attempt fails. The caller must hold c.mu.
func (c *keyCache) refreshLocked(ctx context.Context) {
	c.lastAttempt = c.now()

	set, err := c.fetch(ctx)
	if err != nil {
		// Deliberately does not touch c.set. See the type comment.
		c.lastErr = err
		return
	}
	c.set = set
	c.fetchedAt = c.now()
	c.lastErr = nil
}

func (c *keyCache) fetch(ctx context.Context) (*jose.JSONWebKeySet, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", c.url, err)
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: unexpected status %s", c.url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.url, err)
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("parse key set from %s: %w", c.url, err)
	}
	if len(set.Keys) == 0 {
		// An empty set would replace a working one with nothing, and every
		// later verification would fail with "no published key" — pointing at
		// the token rather than at the endpoint that served nothing.
		return nil, fmt.Errorf("key set from %s contains no keys", c.url)
	}
	return &set, nil
}

// selectKey finds the key named by kid that can verify an alg signature.
//
// The algorithm check is not redundant with the verifier's allow-list. That
// list constrains what the token may ask for; this constrains what the
// published key agrees to be used for. A key published as ES256 must not
// verify a signature claiming some other algorithm, even one otherwise
// allowed, because the issuer's declared intent for that key is part of the
// key set and not the token's to reinterpret.
//
// A key that declares no algorithm is accepted for any allowed one: the field
// is optional in RFC 7517 and plenty of issuers omit it. The key type still
// has to suit the algorithm, which go-jose enforces when it verifies.
func selectKey(set *jose.JSONWebKeySet, kid, alg string) (jose.JSONWebKey, bool) {
	if set == nil {
		return jose.JSONWebKey{}, false
	}
	for _, k := range set.Key(kid) {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if k.Algorithm != "" && k.Algorithm != alg {
			continue
		}
		return k, true
	}
	return jose.JSONWebKey{}, false
}
