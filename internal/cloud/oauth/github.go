package oauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// GitHub's endpoints, from its OAuth app documentation. Constants, not
// configuration: one setting would redirect every sign-in.
const (
	githubAuthURL   = "https://github.com/login/oauth/authorize"
	githubTokenURL  = "https://github.com/login/oauth/access_token"
	githubUserURL   = "https://api.github.com/user"
	githubEmailsURL = "https://api.github.com/user/emails"
)

// githubScope asks for addresses and nothing else. `read:user` grants the whole
// profile and `user` adds the ability to follow accounts; the display name
// comes back on /user with no scope at all.
const githubScope = "user:email"

// GitHub signs people in with a GitHub account.
type GitHub struct {
	ClientID     string
	ClientSecret string

	// Unexported, so only this package's tests move them. Not configuration:
	// a settable token endpoint sends the client secret and every
	// authorization code wherever it points.
	client                       *http.Client
	tokenURL, userURL, emailsURL string
}

// NewGitHub builds a configured provider.
func NewGitHub(clientID, clientSecret string) *GitHub {
	return &GitHub{
		ClientID: clientID, ClientSecret: clientSecret,
		client:    newClient(),
		tokenURL:  githubTokenURL,
		userURL:   githubUserURL,
		emailsURL: githubEmailsURL,
	}
}

func (g *GitHub) Name() string { return "github" }

func (g *GitHub) http() *http.Client {
	if g.client == nil {
		return newClient()
	}
	return g.client
}

func (g *GitHub) AuthCodeURL(state, challenge, redirectURI string) string {
	q := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {githubScope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return githubAuthURL + "?" + q.Encode()
}

// Identify exchanges the code and reads the account's verified primary address.
//
// Two calls: /user's `email` field is the public profile address, freely typed,
// unverified and empty for most accounts. The flagged address is on
// /user/emails.
func (g *GitHub) Identify(ctx context.Context, code, verifier, redirectURI string) (*Identity, error) {
	token, err := exchange(ctx, g.http(), g.tokenURL,
		g.ClientID, g.ClientSecret, code, verifier, redirectURI)
	if err != nil {
		return nil, err
	}

	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := get(ctx, g.http(), g.userURL, token, &profile); err != nil {
		return nil, err
	}
	if profile.ID == 0 {
		return nil, errNoSubject
	}

	var addresses []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := get(ctx, g.http(), g.emailsURL, token, &addresses); err != nil {
		return nil, err
	}

	// Primary AND verified. Not the first verified one: an account can verify
	// several addresses, and picking one GitHub does not consider primary would
	// mean the Cloud account created depends on the order of an API response.
	email := ""
	for _, a := range addresses {
		if a.Primary && a.Verified {
			email = a.Email
			break
		}
	}
	if email == "" {
		return nil, ErrNoVerifiedEmail
	}

	name := profile.Name
	if name == "" {
		name = profile.Login
	}
	return &Identity{
		// The numeric id, not the login: a login can be changed and reused.
		Subject: itoa(profile.ID),
		Email:   strings.ToLower(email),
		Name:    name,
	}, nil
}
