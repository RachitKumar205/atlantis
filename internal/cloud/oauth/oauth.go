// Package oauth signs people in through GitHub and Google.
//
// # What a provider is allowed to tell Cloud
//
// Exactly three things: a stable subject, a verified email address, and a
// display name. Not an access token, not a refresh token, not a scope — Cloud
// reads the profile once during the callback and discards everything else,
// because it never acts on a user's behalf at the provider. Storing a token
// would mean holding a credential to somebody else's GitHub account for no
// purpose, and it would need sealing, rotating and revoking.
//
// # Why the verified-email rule lives in here
//
// Identify returns an address only when the provider says the user proved
// control of it, and ErrNoVerifiedEmail otherwise. That check is inside each
// provider rather than in the handler on purpose. A handler that reads
// `identity.Email` has no way to know whether it was verified, and the version
// of that handler which forgets to ask looks exactly like the one that
// remembers — right up until somebody registers an unverified address matching
// a Cloud account. Here, there is no shape in which an unverified address
// reaches the caller.
//
// # Why not golang.org/x/oauth2
//
// The library's substance is token storage, refresh and transport wrapping, and
// Cloud does none of those. What is left is one form POST and a JSON decode,
// which is what this package is. Revisit if provider tokens ever get stored.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// itoa formats a provider's numeric account id for storage as text.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// ErrNoVerifiedEmail reports an account Cloud will not sign in.
//
// Either the provider has no address for the account, or it has one it will not
// vouch for. Both are refusals rather than a prompt for the user to type an
// address: `cloud.users.email` is unique, so an address nobody proved is a way
// to collide with somebody who did.
var ErrNoVerifiedEmail = errors.New("no verified email address at the provider")

// errNoSubject reports a provider response with no stable identifier.
//
// Without a subject there is nothing to key cloud.identities on, and falling
// back to the email would tie the link to a value people change.
var errNoSubject = errors.New("provider returned no account identifier")

// Identity is everything a provider is trusted to say.
type Identity struct {
	// Subject is the provider's stable identifier. Not the email, which people
	// change — a subject that moved would silently detach a linked account.
	Subject string

	// Email is verified at the provider. Never populated otherwise.
	Email string

	Name string
}

// Provider is one place people can sign in from.
//
// The same shape as mail.Mailer and secrets.Keyring: an interface with a real
// implementation, so tests substitute a fake at the call site rather than
// reaching the network.
type Provider interface {
	// Name is the value stored in cloud.identities.provider. It has to match
	// the CHECK constraint in migrations/cloud/0001.
	Name() string

	// AuthCodeURL is where the browser is sent.
	AuthCodeURL(state, challenge, redirectURI string) string

	// Identify exchanges a callback code for who the user is.
	//
	// One call, doing both the token exchange and the profile read, because the
	// two are never useful apart — and because a token that escaped this
	// function would be a credential with nowhere to live.
	Identify(ctx context.Context, code, verifier, redirectURI string) (*Identity, error)
}

// httpTimeout bounds every outbound call.
//
// A provider that accepts a connection and never answers would otherwise hold a
// request open until the client gives up, and the person sees a browser that
// hangs after consenting rather than an error.
const httpTimeout = 10 * time.Second

// maxBody caps what a provider can make this process allocate.
//
// The responses here are a few hundred bytes. The cap is not sized to them, it
// is sized to be obviously harmless: a provider — or something answering in its
// place — should not be able to choose how much memory Cloud spends.
const maxBody = 1 << 20

// NewState mints the CSRF value and the PKCE verifier for one sign-in attempt.
//
// Both are 256 bits from crypto/rand, matching the session token in
// internal/cloud/store. The verifier's length is inside RFC 7636's 43-128
// character range as base64url.
func NewState() (state, verifier string, err error) {
	if state, err = randomToken(); err != nil {
		return "", "", err
	}
	if verifier, err = randomToken(); err != nil {
		return "", "", err
	}
	return state, verifier, nil
}

// Challenge derives the S256 PKCE challenge from a verifier.
//
// Both providers advertise S256 and neither needs the `plain` fallback, so
// there is no method to negotiate and no branch to get wrong.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// exchange posts an authorization code and returns the access token.
//
// Shared by both providers because the request is the same one: RFC 6749's
// authorization_code grant, with the PKCE verifier attached. Only the endpoint
// and the credentials differ.
func exchange(ctx context.Context, client *http.Client,
	tokenURL, clientID, clientSecret, code, verifier, redirectURI string,
) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub answers form-encoded unless asked otherwise; Google always answers
	// JSON and ignores this. Sending it to both keeps one decode path.
	req.Header.Set("Accept", "application/json")

	body, err := do(client, req)
	if err != nil {
		return "", err
	}

	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	if out.Error != "" {
		// The provider's description is deliberately not passed on to the user.
		// It is attacker-influenceable text, and the person consenting cannot
		// act on it anyway.
		return "", fmt.Errorf("token exchange refused: %s", out.Error)
	}
	if out.AccessToken == "" {
		return "", errors.New("token exchange returned no access token")
	}
	return out.AccessToken, nil
}

// get reads a provider API endpoint with a bearer token.
func get(ctx context.Context, client *http.Client, endpoint, token string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	body, err := do(client, req)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

// do sends a request and reads a bounded body.
func do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not included. It comes from a third party, would end up
		// in a log, and on some providers carries the token that was just sent.
		return nil, fmt.Errorf("%s %s: %s", req.Method, req.URL.Host, resp.Status)
	}
	return body, nil
}

func newClient() *http.Client { return &http.Client{Timeout: httpTimeout} }
