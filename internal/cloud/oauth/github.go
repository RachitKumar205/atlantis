package oauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// GitHub's endpoints, from its OAuth app documentation.
//
// Constants rather than configuration: these are not per-deployment, and a
// deployment that could point them elsewhere would be a deployment where
// changing one environment variable redirects every sign-in to an attacker.
const (
	githubAuthURL   = "https://github.com/login/oauth/authorize"
	githubTokenURL  = "https://github.com/login/oauth/access_token"
	githubUserURL   = "https://api.github.com/user"
	githubEmailsURL = "https://api.github.com/user/emails"
)

// githubScope asks for the addresses and nothing else.
//
// Not `read:user`, which grants the whole profile, and not `user`, which
// includes the ability to follow accounts. Cloud needs one verified address and
// a display name, and the name comes back on /user without any scope at all.
const githubScope = "user:email"

// GitHub signs people in with a GitHub account.
type GitHub struct {
	ClientID     string
	ClientSecret string

	// Endpoints and the HTTP client, unexported so only this package can move
	// them — which in practice means only its tests.
	//
	// They are deliberately not configuration. A deployment able to point the
	// token endpoint somewhere else is a deployment where one environment
	// variable sends this client secret, and every authorization code, to
	// whoever set it.
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
// Two calls, because GitHub's /user endpoint reports a `email` field that is
// the account's PUBLIC profile email — which the user types in freely, is not
// verified, and is empty for most accounts. The address that means anything is
// on /user/emails, flagged.
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
		// The numeric id, not the login. A login can be changed and reused by
		// somebody else; the id cannot.
		Subject: itoa(profile.ID),
		Email:   strings.ToLower(email),
		Name:    name,
	}, nil
}
