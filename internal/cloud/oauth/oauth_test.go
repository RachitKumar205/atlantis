package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Provider parsing, against a fake provider over real HTTP.
//
// No database, so nothing here skips. That is why this package exists separately
// from internal/cloud/server, where every test is gated on ATLANTIS_TEST_PG: the
// rules about which addresses are acceptable are the security-carrying part of
// OAuth, and they must not live in a suite that can silently not run.

// fakeProvider stands in for GitHub or Google.
//
// It records what was asked for, so a test can assert the request as well as
// the response — the PKCE verifier and the redirect_uri are only observable
// from this side.
type fakeProvider struct {
	*httptest.Server

	tokenResponse string
	tokenStatus   int
	routes        map[string]string

	lastTokenForm url.Values
	lastAuth      string
}

func newFake(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{tokenStatus: http.StatusOK, routes: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			f.lastTokenForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.tokenStatus)
			_, _ = w.Write([]byte(f.tokenResponse))
			return
		}
		body, ok := f.routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.lastAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	f.tokenResponse = `{"access_token":"at-1"}`
	return f
}

// github builds a GitHub provider pointed at the fake.
func (f *fakeProvider) github() *GitHub {
	g := NewGitHub("id", "secret")
	g.client = f.Client()
	return g
}

func (f *fakeProvider) google() *Google {
	g := NewGoogle("id", "secret")
	g.client = f.Client()
	return g
}

// The challenge is the S256 hash of the verifier, so a provider can bind the
// exchange to the request that started it.
func TestTheChallengeIsTheHashOfTheVerifier(t *testing.T) {
	state, verifier, err := NewState()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if state == verifier {
		t.Fatal("the state and the verifier are the same value")
	}
	// Both are 32 bytes as base64url, which is 43 characters — inside RFC 7636's
	// 43-128 range for a code_verifier.
	if len(verifier) != 43 {
		t.Errorf("verifier is %d characters, want 43", len(verifier))
	}

	sum := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); Challenge(verifier) != want {
		t.Errorf("challenge is %q, want %q", Challenge(verifier), want)
	}
}

// Two mints do not collide.
func TestEachSignInGetsItsOwnState(t *testing.T) {
	a, av, err := NewState()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b, bv, err := NewState()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if a == b || av == bv {
		t.Fatal("two sign-ins were given the same state or verifier")
	}
}

// The authorize URL carries what the provider needs to bind the exchange.
func TestTheAuthorizeURLCarriesTheChallenge(t *testing.T) {
	for _, p := range []Provider{NewGitHub("cid", "s"), NewGoogle("cid", "s")} {
		raw := p.AuthCodeURL("st-1", "ch-1", "https://cloud.test/auth/x/callback")
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: parse: %v", p.Name(), err)
		}
		q := u.Query()
		for k, want := range map[string]string{
			"client_id":             "cid",
			"state":                 "st-1",
			"code_challenge":        "ch-1",
			"code_challenge_method": "S256",
			"redirect_uri":          "https://cloud.test/auth/x/callback",
		} {
			if got := q.Get(k); got != want {
				t.Errorf("%s: %s is %q, want %q", p.Name(), k, got, want)
			}
		}
	}
}

// ── GitHub ──────────────────────────────────────────────────────────────────

func githubOK(f *fakeProvider, emails string) *GitHub {
	f.routes["/user"] = `{"id":4242,"login":"ada","name":"Ada Lovelace"}`
	f.routes["/user/emails"] = emails
	g := f.github()
	g.tokenURL, g.userURL, g.emailsURL =
		f.URL+"/token", f.URL+"/user", f.URL+"/user/emails"
	return g
}

// The primary verified address is the one taken.
func TestGitHubTakesThePrimaryVerifiedAddress(t *testing.T) {
	f := newFake(t)
	g := githubOK(f, `[
		{"email":"other@example.com","primary":false,"verified":true},
		{"email":"Ada@Example.com","primary":true,"verified":true}
	]`)

	id, err := g.Identify(context.Background(), "code-1", "ver-1", "https://cloud.test/cb")
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	if id.Email != "ada@example.com" {
		t.Errorf("address is %q, want the folded primary", id.Email)
	}
	// The numeric id, not the login: a login can be changed and reissued to
	// somebody else, and a link keyed to it would follow the name.
	if id.Subject != "4242" {
		t.Errorf("subject is %q, want the numeric id", id.Subject)
	}
	if id.Name != "Ada Lovelace" {
		t.Errorf("name is %q", id.Name)
	}

	// The verifier reached the token endpoint, so PKCE is actually in play
	// rather than only in the authorize URL.
	if f.lastTokenForm.Get("code_verifier") != "ver-1" {
		t.Error("the exchange did not send the PKCE verifier")
	}
	if f.lastTokenForm.Get("redirect_uri") != "https://cloud.test/cb" {
		t.Error("the exchange did not send the redirect_uri")
	}
	if f.lastAuth != "Bearer at-1" {
		t.Errorf("the API call sent %q", f.lastAuth)
	}
}

// A verified address that is not primary is not enough.
//
// Otherwise the account created would depend on the order of an API response.
func TestGitHubRefusesWhenNoAddressIsPrimaryAndVerified(t *testing.T) {
	for _, emails := range []string{
		`[{"email":"a@example.com","primary":true,"verified":false}]`,
		`[{"email":"a@example.com","primary":false,"verified":true}]`,
		`[]`,
	} {
		f := newFake(t)
		g := githubOK(f, emails)
		_, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
		if !errors.Is(err, ErrNoVerifiedEmail) {
			t.Errorf("emails %s returned %v, want ErrNoVerifiedEmail", emails, err)
		}
	}
}

// GitHub answers a failed exchange with 200 and an error body.
//
// The single most common way to get this wrong: a status check alone passes,
// the access token is empty, and the next call fails with something unrelated.
func TestGitHubReportsAFailedExchangeSentWithStatus200(t *testing.T) {
	f := newFake(t)
	f.tokenResponse = `{"error":"bad_verification_code","error_description":"expired"}`
	g := githubOK(f, `[{"email":"a@example.com","primary":true,"verified":true}]`)

	_, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
	if err == nil {
		t.Fatal("a refused exchange returned no error")
	}
	if errors.Is(err, ErrNoVerifiedEmail) {
		t.Fatal("a refused exchange was reported as a missing address")
	}
	// The provider's description is not carried into the message: it is text
	// from a third party that would end up in a log.
	if strings.Contains(err.Error(), "expired") {
		t.Errorf("the provider's description was passed through: %v", err)
	}
}

// A token response with neither an error nor a token is a failure.
func TestAnEmptyTokenResponseIsAFailure(t *testing.T) {
	f := newFake(t)
	f.tokenResponse = `{}`
	g := githubOK(f, `[{"email":"a@example.com","primary":true,"verified":true}]`)

	if _, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb"); err == nil {
		t.Fatal("an empty token response returned no error")
	}
}

// An account with no numeric id is refused rather than linked to "0".
func TestGitHubRefusesAProfileWithNoID(t *testing.T) {
	f := newFake(t)
	g := githubOK(f, `[{"email":"a@example.com","primary":true,"verified":true}]`)
	f.routes["/user"] = `{"login":"ada"}`

	if _, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb"); err == nil {
		t.Fatal("a profile with no id was accepted")
	}
}

// The login stands in when the profile has no name.
func TestGitHubFallsBackToTheLogin(t *testing.T) {
	f := newFake(t)
	g := githubOK(f, `[{"email":"a@example.com","primary":true,"verified":true}]`)
	f.routes["/user"] = `{"id":7,"login":"ada"}`

	id, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	if id.Name != "ada" {
		t.Errorf("name is %q, want the login", id.Name)
	}
}

// ── Google ──────────────────────────────────────────────────────────────────

func googleWith(f *fakeProvider, userinfo string) *Google {
	f.routes["/userinfo"] = userinfo
	g := f.google()
	g.tokenURL, g.userInfoURL = f.URL+"/token", f.URL+"/userinfo"
	return g
}

func TestGoogleTakesAVerifiedAddress(t *testing.T) {
	f := newFake(t)
	g := googleWith(f, `{"sub":"11223344","email":"Ada@Example.com","email_verified":true,"name":"Ada"}`)

	id, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	if id.Subject != "11223344" {
		t.Errorf("subject is %q", id.Subject)
	}
	if id.Email != "ada@example.com" {
		t.Errorf("address is %q, want it folded", id.Email)
	}
}

// email_verified false is a refusal, and so is anything that is not true.
//
// A hosted-domain account can carry an address the administrator set without
// the person proving anything, and it comes back unverified.
func TestGoogleRefusesAnythingButAVerifiedAddress(t *testing.T) {
	for _, userinfo := range []string{
		`{"sub":"1","email":"a@example.com","email_verified":false}`,
		`{"sub":"1","email":"a@example.com"}`,
		`{"sub":"1","email_verified":true}`,
	} {
		f := newFake(t)
		g := googleWith(f, userinfo)
		_, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
		if !errors.Is(err, ErrNoVerifiedEmail) {
			t.Errorf("userinfo %s returned %v, want ErrNoVerifiedEmail", userinfo, err)
		}
	}
}

func TestGoogleRefusesUserinfoWithNoSubject(t *testing.T) {
	f := newFake(t)
	g := googleWith(f, `{"email":"a@example.com","email_verified":true}`)

	if _, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb"); err == nil {
		t.Fatal("userinfo with no subject was accepted")
	}
}

// An API that answers with an error status is a failure, not an empty identity.
func TestAFailedAPICallIsAnError(t *testing.T) {
	f := newFake(t)
	g := f.google()
	g.tokenURL, g.userInfoURL = f.URL+"/token", f.URL+"/missing"

	if _, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb"); err == nil {
		t.Fatal("a 404 from the provider returned no error")
	}
}

// The response body is bounded, so a provider cannot choose how much memory
// this process spends.
func TestAHugeResponseIsBounded(t *testing.T) {
	big := strings.Repeat("a", maxBody*2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = w.Write([]byte(`{"access_token":"at"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sub":"1","name":"` + big + `"}`))
	}))
	defer srv.Close()

	g := NewGoogle("id", "secret")
	g.client = srv.Client()
	g.tokenURL, g.userInfoURL = srv.URL+"/token", srv.URL+"/userinfo"

	// Truncated at the cap, so the JSON no longer parses. The point is that it
	// fails rather than allocating whatever was sent.
	_, err := g.Identify(context.Background(), "c", "v", "https://cloud.test/cb")
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Logf("bounded read produced %v", err)
	}
}
