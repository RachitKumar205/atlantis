package oauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Google's endpoints, taken from its OpenID Connect discovery document.
//
// Hard-coded for the same reason GitHub's are: they are not a per-deployment
// choice, and making them one would turn a single environment variable into a
// way to send every sign-in somewhere else.
const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

// googleScope is the OpenID Connect minimum for an identity.
const googleScope = "openid email profile"

// Google signs people in with a Google account.
type Google struct {
	ClientID     string
	ClientSecret string

	// Unexported for the reason set out on GitHub's equivalent: these are not
	// a deployment's to choose.
	client                *http.Client
	tokenURL, userInfoURL string
}

// NewGoogle builds a configured provider.
func NewGoogle(clientID, clientSecret string) *Google {
	return &Google{
		ClientID: clientID, ClientSecret: clientSecret,
		client:      newClient(),
		tokenURL:    googleTokenURL,
		userInfoURL: googleUserInfoURL,
	}
}

func (g *Google) Name() string { return "google" }

func (g *Google) http() *http.Client {
	if g.client == nil {
		return newClient()
	}
	return g.client
}

func (g *Google) AuthCodeURL(state, challenge, redirectURI string) string {
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {googleScope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return googleAuthURL + "?" + q.Encode()
}

// Identify exchanges the code and reads the userinfo endpoint.
//
// Reads userinfo rather than verifying the id_token the exchange also returns.
// The claims come over TLS from Google's own endpoint either way, and
// verifying locally needs a JWKS cache and a key-rotation path.
func (g *Google) Identify(ctx context.Context, code, verifier, redirectURI string) (*Identity, error) {
	token, err := exchange(ctx, g.http(), g.tokenURL,
		g.ClientID, g.ClientSecret, code, verifier, redirectURI)
	if err != nil {
		return nil, err
	}

	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := get(ctx, g.http(), g.userInfoURL, token, &info); err != nil {
		return nil, err
	}
	if info.Sub == "" {
		return nil, errNoSubject
	}
	// Both halves. A Google Workspace account can carry an address the domain
	// administrator set without the user proving anything, and it comes back
	// with email_verified false.
	if info.Email == "" || !info.EmailVerified {
		return nil, ErrNoVerifiedEmail
	}

	return &Identity{
		Subject: info.Sub,
		Email:   strings.ToLower(info.Email),
		Name:    info.Name,
	}, nil
}
