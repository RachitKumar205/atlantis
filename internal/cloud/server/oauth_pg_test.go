package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/oauth"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Signing in through a provider, end to end against a real database.
//
// The provider is the only fake. Everything else — the cookies, the pending
// login, the second factor, the row-level policies — is the real thing, run as
// a non-superuser role so the boundary is not inert.

// fakeOAuth stands in for GitHub or Google at the server's seam.
//
// Substituted onto f.srv.providers after New() returns, which is the same seam
// mailer, breach and sleep use.
type fakeOAuth struct {
	name string

	// identity is returned by Identify unless err is set.
	identity oauth.Identity
	err      error

	lastState     string
	lastChallenge string
	lastRedirect  string
	lastVerifier  string
}

func (f *fakeOAuth) Name() string { return f.name }

func (f *fakeOAuth) AuthCodeURL(state, challenge, redirectURI string) string {
	f.lastState, f.lastChallenge, f.lastRedirect = state, challenge, redirectURI
	return "https://provider.test/authorize?state=" + state
}

func (f *fakeOAuth) Identify(_ context.Context, _, verifier, _ string) (*oauth.Identity, error) {
	f.lastVerifier = verifier
	if f.err != nil {
		return nil, f.err
	}
	id := f.identity
	return &id, nil
}

// fakeFor installs a fake provider and returns it.
func (f *fixture) fakeFor(name, subject, email, display string) *fakeOAuth {
	p := &fakeOAuth{name: name, identity: oauth.Identity{
		Subject: subject, Email: email, Name: display,
	}}
	f.srv.providers[name] = p
	return p
}

// getWithCookies sends a GET carrying named cookies.
//
// The callback is a top-level navigation from the provider, so every OAuth test
// needs this; the fixture only had a POST equivalent.
func (f *fixture) getWithCookies(t *testing.T, path string, cookies map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = f.nextIP() + ":1234"
	for name, value := range cookies {
		if value != "" {
			r.AddCookie(&http.Cookie{Name: name, Value: value})
		}
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

// startOAuth begins a sign-in and returns the state cookie and the state value
// the provider was handed.
func (f *fixture) startOAuth(t *testing.T, provider string, session string) (cookie, state string) {
	t.Helper()
	rec := f.getWithCookies(t, "/auth/"+provider, map[string]string{sessionCookie: session})
	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	cookie = cookieFrom(rec, oauthCookie)
	if cookie == "" {
		t.Fatal("starting a sign-in set no state cookie")
	}
	p, ok := f.srv.providers[provider].(*fakeOAuth)
	if !ok {
		t.Fatalf("provider %s is not the fake", provider)
	}
	return cookie, p.lastState
}

// callback completes a sign-in with the state the provider was given.
func (f *fixture) callback(t *testing.T, provider, cookie, state string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	cookies := map[string]string{oauthCookie: cookie}
	for k, v := range extra {
		cookies[k] = v
	}
	return f.getWithCookies(t,
		"/auth/"+provider+"/callback?code=code-1&state="+state, cookies)
}

// identityCount reports how many links an address's account has.
//
// Zero when the account does not exist, which is what the refusal tests need:
// "no identity row was written" has to be checked against the table, not
// inferred from a status code.
func (f *fixture) identityCount(t *testing.T, email string) int {
	t.Helper()
	ctx := context.Background()
	u, err := f.db.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("look up %s: %v", email, err)
	}
	linked, err := f.db.IdentitiesOf(ctx, u.ID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	return len(linked)
}

// A provider sign-in produces a pending login, never a session.
//
// This is the property the whole step is arranged around. A provider vouching
// for somebody is one factor; if the callback issued a session, the second
// factor would be decoration and OAuth would be the way around it.
func TestAProviderSignInIsNotASession(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-1", "new@example.com", "New Person")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)

	// 303 back to the sign-in application; see handOff.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if got := cookieFrom(rec, sessionCookie); got != "" {
		t.Fatal("the callback issued a session, so a provider alone signs somebody in")
	}
	pending := cookieFrom(rec, pendingCookie)
	if pending == "" {
		t.Fatal("the callback issued no pending login")
	}
	// And the pending token is not a session anywhere else either.
	if _, err := f.db.SessionUser(context.Background(), pending); !errors.Is(err, store.ErrNoSession) {
		t.Fatalf("a pending token resolved as a session: %v", err)
	}
	// A brand-new account has no factor, so it may enrol and nothing else.
	// That now travels in the redirect rather than in a page of prose.
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "next=enrol") {
		t.Errorf("the callback did not say what happens next: %q", loc)
	}
}

// A first sign-in creates a verified, password-less account with its link.
func TestAFirstProviderSignInCreatesAnAccount(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("google", "goog-9", "fresh@example.com", "Fresh")

	cookie, state := f.startOAuth(t, "google", "")
	if rec := f.callback(t, "google", cookie, state, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}

	u, err := f.db.UserByEmail(context.Background(), "fresh@example.com")
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if u.EmailVerifiedAt == nil {
		t.Error("the account is unverified, so it can never sign in")
	}
	if u.HasPassword() {
		t.Error("the account has a password")
	}
	if n := f.identityCount(t, "fresh@example.com"); n != 1 {
		t.Errorf("the account has %d links, want 1", n)
	}
}

// A second sign-in finds the account rather than making another.
func TestASecondProviderSignInFindsTheSameAccount(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-2", "repeat@example.com", "Repeat")

	for i := 0; i < 2; i++ {
		cookie, state := f.startOAuth(t, "github", "")
		if rec := f.callback(t, "github", cookie, state, nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("callback %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if n := f.identityCount(t, "repeat@example.com"); n != 1 {
		t.Errorf("two sign-ins produced %d links", n)
	}
}

// A provider-verified address matching an account WITH a second factor links,
// and still has to present that factor.
func TestAutoLinkingNeedsTheExistingFactor(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "member@example.com")
	f.enrol(t, "member@example.com")
	f.fakeFor("github", "gh-3", "member@example.com", "Member")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if n := f.identityCount(t, "member@example.com"); n != 1 {
		t.Fatalf("the link was not made: %d rows", n)
	}
	if cookieFrom(rec, sessionCookie) != "" {
		t.Fatal("auto-linking produced a session")
	}

	// The pending login may NOT enrol — it has to present the factor already
	// there. If it could enrol, whoever controls the provider account would set
	// a new second factor on somebody else's account.
	pending := cookieFrom(rec, pendingCookie)
	p, err := f.db.PendingLoginFor(context.Background(), pending)
	if err != nil {
		t.Fatalf("pending login: %v", err)
	}
	if p.MayEnrol {
		t.Fatal("the pending login could enrol a new factor over the existing one")
	}
	enrol := f.postWithCookie(t, "/api/auth/2fa/enrol/begin", `{}`, pendingCookie, pending)
	if enrol.Code == http.StatusOK {
		t.Fatal("a provider sign-in reached enrolment on an account that already had a factor")
	}
}

// A provider-verified address matching a FACTORLESS account is refused, and
// nothing is written.
//
// The refusal is what makes auto-linking acceptable at all: with no factor
// behind it, a provider's word about an address is the only thing between an
// attacker and the account, and the enrolling pending login they would receive
// is account takeover.
func TestAutoLinkingIntoAFactorlessAccountIsRefused(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "bare@example.com")
	f.fakeFor("github", "gh-4", "bare@example.com", "Bare")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)

	// Still a 409 page: this is the factorless-account refusal, not the
	// already-claimed one, and it is not a step in a flow the sign-in app can
	// continue. Moving it into the app is W4's problem.
	if rec.Code != http.StatusConflict {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	// Both halves. The status alone would pass if the row had been written and
	// the refusal came afterwards.
	if n := f.identityCount(t, "bare@example.com"); n != 0 {
		t.Fatalf("the refusal still wrote %d identity rows", n)
	}
	if cookieFrom(rec, pendingCookie) != "" {
		t.Fatal("the refusal handed out a pending login")
	}
	if cookieFrom(rec, sessionCookie) != "" {
		t.Fatal("the refusal handed out a session")
	}
}

// An unverified account gets an answer it can act on.
//
// The factorless refusal says "sign in with your password", which handleLogin
// refuses for an unverified address — so an account in that state would be told
// to do the one thing that cannot work.
func TestAnUnverifiedAccountIsPointedAtVerification(t *testing.T) {
	f := newFixture(t)
	// Signed up, never followed the link.
	if rec := f.post(t, "/api/auth/signup",
		`{"email":"pending@example.com","password":"`+goodPassword+`","first_name":"","last_name":""}`); rec.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body.String())
	}
	f.fakeFor("google", "goog-5", "pending@example.com", "Pending")

	cookie, state := f.startOAuth(t, "google", "")
	rec := f.callback(t, "google", cookie, state, nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "verif") {
		t.Errorf("the answer does not mention verification: %s", rec.Body.String())
	}
	if n := f.identityCount(t, "pending@example.com"); n != 0 {
		t.Errorf("the refusal wrote %d identity rows", n)
	}
}

// An address the provider will not vouch for is refused.
func TestAnUnverifiedProviderAddressIsRefused(t *testing.T) {
	f := newFixture(t)
	p := f.fakeFor("github", "gh-6", "unverified@example.com", "Nobody")
	p.err = oauth.ErrNoVerifiedEmail

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.db.UserByEmail(context.Background(), "unverified@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an account was created for an address nobody vouched for")
	}
}

// A linked provider account signs in as its owner, whatever address the
// provider reports.
//
// The subject is what the link is keyed by; provider_email is display-only.
// Were the email consulted, somebody who could set an address at a provider
// would decide which Cloud account a subject resolves to — which is the exact
// hazard migration 0001 records as its reason for not matching on it.
func TestASignInFollowsTheSubjectAndNotTheAddress(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	owner := f.verifiedAccount(t, "owner@example.com")
	f.enrol(t, "owner@example.com")
	if err := f.db.LinkIdentity(ctx, owner.ID, "github", "shared", "owner@example.com"); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	// A second account whose address the provider now claims, while reporting
	// the subject that belongs to the first.
	other := f.verifiedAccount(t, "other@example.com")
	f.enrol(t, "other@example.com")
	f.fakeFor("github", "shared", "other@example.com", "Other")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}

	pending := cookieFrom(rec, pendingCookie)
	p, err := f.db.PendingLoginFor(ctx, pending)
	if err != nil {
		t.Fatalf("pending login: %v", err)
	}
	if p.UserID != owner.ID {
		t.Fatalf("the sign-in resolved to %s; the subject belongs to %s", p.UserID, owner.ID)
	}
	if p.UserID == other.ID {
		t.Fatal("the provider's address decided which account was signed in to")
	}

	// The other account gained nothing.
	linked, err := f.db.IdentitiesOf(ctx, other.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 0 {
		t.Fatal("the account named by the provider's address was given the link")
	}
}

// A provider account cannot be connected to a second Cloud account.
//
// Reached through the linking route rather than sign-in: a sign-in resolves the
// subject first and finds the existing owner, so the refusal below is the one a
// person hits when they connect a provider account somebody else already has.
func TestConnectingAClaimedProviderAccountIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	owner := f.verifiedAccount(t, "holder@example.com")
	if err := f.db.LinkIdentity(ctx, owner.ID, "github", "contested", ""); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	f.verifiedAccount(t, "wanter@example.com")
	session, _, _ := f.enrol(t, "wanter@example.com")
	f.fakeFor("github", "contested", "wanter@example.com", "Wanter")

	cookie, state := f.startOAuth(t, "github", session)
	rec := f.callback(t, "github", cookie, state,
		map[string]string{sessionCookie: session})

	// Back to the account screen with the reason, not a 409 dead end.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "outcome=claimed") {
		t.Errorf("the refusal does not say why: %q", loc)
	}
	stillOwner, err := f.db.UserByIdentity(ctx, "github", "contested")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if stillOwner.ID != owner.ID {
		t.Fatal("the provider account changed hands")
	}
}

// A state that does not match the cookie is refused.
func TestAMismatchedStateIsRefused(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-7", "state@example.com", "State")

	cookie, _ := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, "some-other-state", nil)

	if rec.Code == http.StatusOK {
		t.Fatal("a callback with the wrong state completed")
	}
	if cookieFrom(rec, pendingCookie) != "" {
		t.Fatal("a mismatched state produced a pending login")
	}
	if _, err := f.db.UserByEmail(context.Background(), "state@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a mismatched state created an account")
	}
}

// A callback with no state cookie is refused.
//
// This is the login-CSRF case: an attacker who can make a browser follow a URL
// cannot make it produce a cookie only this server sets.
func TestACallbackWithNoStateCookieIsRefused(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-8", "nocookie@example.com", "No Cookie")

	_, state := f.startOAuth(t, "github", "")
	rec := f.getWithCookies(t, "/auth/github/callback?code=c&state="+state, nil)

	if rec.Code == http.StatusOK {
		t.Fatal("a callback with no state cookie completed")
	}
	if _, err := f.db.UserByEmail(context.Background(), "nocookie@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a callback with no state cookie created an account")
	}
}

// A state minted for one provider cannot be spent at another's callback.
func TestAStateFromAnotherProviderIsRefused(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-9", "cross@example.com", "Cross")
	f.fakeFor("google", "goog-9", "cross@example.com", "Cross")

	cookie, state := f.startOAuth(t, "google", "")
	rec := f.callback(t, "github", cookie, state, nil)

	if rec.Code == http.StatusOK {
		t.Fatal("a state minted for Google completed GitHub's callback")
	}
}

// The deadline is enforced here, not left to the browser.
//
// MaxAge is a request a browser honours; anything that kept a copy of the value
// could present it afterwards. So the expiry travels inside the value.
func TestAnExpiredStateIsRefusedEvenIfPresented(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-10", "stale@example.com", "Stale")

	stale := oauthState{
		Provider: "github", Intent: intentSignIn,
		State: "s-1", Verifier: "v-1",
		Expires: time.Now().Add(-time.Minute),
	}.encode()

	rec := f.callback(t, "github", stale, "s-1", nil)
	if rec.Code == http.StatusOK {
		t.Fatal("an expired state completed a sign-in")
	}
}

// The state cookie is spent by the callback, so it cannot be replayed.
func TestAStateCookieIsSpentByTheCallback(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-11", "once@example.com", "Once")

	cookie, state := f.startOAuth(t, "github", "")
	first := f.callback(t, "github", cookie, state, nil)
	if first.Code != http.StatusSeeOther {
		t.Fatalf("first callback: %d %s", first.Code, first.Body.String())
	}
	// The response clears it. A browser would not send it again; this asserts
	// the clear was issued, which is what makes that true.
	cleared := false
	for _, c := range first.Result().Cookies() {
		if c.Name == oauthCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the callback did not clear the state cookie, so it can be replayed")
	}
}

// The PKCE challenge the provider was given is the hash of the verifier the
// exchange later used.
func TestThePKCEVerifierSurvivesTheRoundTrip(t *testing.T) {
	f := newFixture(t)
	p := f.fakeFor("github", "gh-12", "pkce@example.com", "PKCE")

	cookie, state := f.startOAuth(t, "github", "")
	if rec := f.callback(t, "github", cookie, state, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if p.lastVerifier == "" {
		t.Fatal("the exchange was given no verifier")
	}
	if oauth.Challenge(p.lastVerifier) != p.lastChallenge {
		t.Fatal("the verifier sent to the exchange does not hash to the challenge sent to authorize")
	}
}

// The redirect_uri comes from configuration, not from the request.
//
// A Host header is attacker-controlled, and a redirect_uri derived from it
// would ask the provider to deliver the authorization code somewhere else.
func TestTheRedirectURIIgnoresTheHostHeader(t *testing.T) {
	f := newFixture(t)
	p := f.fakeFor("github", "gh-13", "host@example.com", "Host")

	r := httptest.NewRequest(http.MethodGet, "/auth/github", nil)
	r.Host = "evil.example"
	r.RemoteAddr = f.nextIP() + ":1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("start: %d", rec.Code)
	}
	if !strings.HasPrefix(p.lastRedirect, "https://cloud.test/") {
		t.Fatalf("redirect_uri is %q; it followed the Host header", p.lastRedirect)
	}
}

// A provider that refused, or a user who declined, is a page rather than a fault.
func TestADeclinedConsentIsNotAnError(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-14", "declined@example.com", "Declined")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.getWithCookies(t,
		"/auth/github/callback?error=access_denied&state="+state,
		map[string]string{oauthCookie: cookie})

	if rec.Code >= 500 {
		t.Fatalf("a declined consent produced %d", rec.Code)
	}
	if cookieFrom(rec, pendingCookie) != "" {
		t.Fatal("a declined consent produced a pending login")
	}
}

// A provider with no credentials has no routes at all.
func TestAnUnconfiguredProviderDoesNotExist(t *testing.T) {
	f := newFixtureWithoutProviders(t)

	for _, path := range []string{"/auth/github", "/auth/github/callback", "/auth/google"} {
		if rec := f.get(t, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404 — an unconfigured provider should "+
				"not exist rather than fail at the redirect", path, rec.Code)
		}
	}
}

// Removing a provider's credentials does not strand the people who linked it.
func TestConnectedAccountsSurviveAProviderBeingTurnedOff(t *testing.T) {
	f := newFixtureWithoutProviders(t)
	ctx := context.Background()

	u := f.verifiedAccount(t, "stranded@example.com")
	if err := f.db.LinkIdentity(ctx, u.ID, "github", "gone", ""); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	session, _, _ := f.enrol(t, "stranded@example.com")

	rec := f.getWithCookies(t, "/api/account/identities",
		map[string]string{sessionCookie: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github") {
		t.Fatalf("a link to a turned-off provider is not listed: %s", rec.Body.String())
	}

	// And it can still be removed — there is a password behind it.
	un := f.postWithCookie(t, "/api/account/identities/github/unlink", `{}`,
		sessionCookie, session)
	if un.Code != http.StatusOK {
		t.Fatalf("unlink: %d %s", un.Code, un.Body.String())
	}
}

// Reading and removing connections needs a session, not a pending login.
func TestConnectedAccountsNeedASession(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "half@example.com")

	login := f.post(t, "/api/auth/login",
		`{"email":"half@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)
	if pending == "" {
		t.Fatal("login set no pending cookie")
	}

	rec := f.getWithCookies(t, "/api/account/identities",
		map[string]string{pendingCookie: pending})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a pending login read connected accounts: %d %s", rec.Code, rec.Body.String())
	}
	// And presenting it as a session does not work either.
	rec = f.getWithCookies(t, "/api/account/identities",
		map[string]string{sessionCookie: pending})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a pending token used as a session read connected accounts: %d", rec.Code)
	}
}

// The only way in cannot be removed.
func TestTheLastSignInMethodCannotBeUnlinked(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-15", "solo@example.com", "Solo")

	// Sign in, then enrol, which is what turns the pending login into a session.
	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	pending := cookieFrom(rec, pendingCookie)
	session := f.finishEnrolment(t, pending)

	un := f.postWithCookie(t, "/api/account/identities/github/unlink", `{}`,
		sessionCookie, session)
	if un.Code != http.StatusConflict {
		t.Fatalf("unlink returned %d %s, want 409", un.Code, un.Body.String())
	}

	// Still there.
	u, err := f.db.UserByEmail(context.Background(), "solo@example.com")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}
	linked, err := f.db.IdentitiesOf(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 {
		t.Fatal("the refused unlink removed the only way in")
	}
}

// The listing says whether each connection can be removed, so a client can
// explain a refusal before making it.
func TestTheListingSaysWhatCanBeDisconnected(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-16", "listing@example.com", "Listing")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)
	session := f.finishEnrolment(t, cookieFrom(rec, pendingCookie))

	list := f.getWithCookies(t, "/api/account/identities",
		map[string]string{sessionCookie: session})
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var out struct {
		Identities []struct {
			Provider      string `json:"provider"`
			CanDisconnect bool   `json:"can_disconnect"`
		} `json:"identities"`
		HasPassword bool `json:"has_password"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Identities) != 1 {
		t.Fatalf("%d connections listed, want 1", len(out.Identities))
	}
	if out.HasPassword {
		t.Error("an account created from a provider reports having a password")
	}
	if out.Identities[0].CanDisconnect {
		t.Error("the only way in is reported as removable")
	}
	// The provider subject is an opaque key nobody can act on, and it is what
	// the link is keyed by. It has no business in a response.
	if strings.Contains(list.Body.String(), "gh-16") {
		t.Error("the listing publishes the provider subject")
	}
}

// finishEnrolment takes a pending login through TOTP enrolment to a session.
func (f *fixture) finishEnrolment(t *testing.T, pending string) string {
	t.Helper()
	if pending == "" {
		t.Fatal("no pending login to finish")
	}
	begin := f.postWithCookie(t, "/api/auth/2fa/enrol/begin", `{}`, pendingCookie, pending)
	if begin.Code != http.StatusOK {
		t.Fatalf("enrol begin: %d %s", begin.Code, begin.Body.String())
	}
	var started struct{ Secret string }
	if err := json.Unmarshal(begin.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode: %v", err)
	}
	finish := f.postWithCookie(t, "/api/auth/2fa/enrol/finish",
		`{"code":`+jsonString(codeAt(t, started.Secret, time.Now()))+`}`,
		pendingCookie, pending)
	if finish.Code != http.StatusOK {
		t.Fatalf("enrol finish: %d %s", finish.Code, finish.Body.String())
	}
	session := cookieFrom(finish, sessionCookie)
	if session == "" {
		t.Fatal("finishing enrolment produced no session")
	}
	return session
}

// Connecting a provider while signed in attaches it to that account and does
// not start a new sign-in.
func TestLinkingAttachesToTheSignedInAccount(t *testing.T) {
	f := newFixture(t)
	u := f.verifiedAccount(t, "linker@example.com")
	session, _, _ := f.enrol(t, "linker@example.com")
	// A different address at the provider, so a link that resolved by email
	// rather than by session would attach to the wrong place.
	f.fakeFor("github", "gh-17", "elsewhere@example.com", "Elsewhere")

	cookie, state := f.startOAuth(t, "github", session)
	rec := f.callback(t, "github", cookie, state,
		map[string]string{sessionCookie: session})
	// The link branch returns to the account screen now, where it used to end
	// on a text/plain page with no way back.
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}

	linked, err := f.db.IdentitiesOf(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("the signed-in account has %d links, want 1", len(linked))
	}
	// No second account for the provider's address.
	if _, err := f.db.UserByEmail(context.Background(), "elsewhere@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("linking created an account for the provider's address")
	}
	// A link is not a sign-in, so it produces no pending login.
	if cookieFrom(rec, pendingCookie) != "" {
		t.Fatal("linking started a sign-in")
	}
}

// A link whose session went away is refused rather than attached to whoever is
// signed in now.
func TestLinkingWithoutTheSessionIsRefused(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "gone@example.com")
	session, _, _ := f.enrol(t, "gone@example.com")
	f.fakeFor("github", "gh-18", "provider@example.com", "Provider")

	cookie, state := f.startOAuth(t, "github", session)
	// The callback arrives with the state but no session.
	rec := f.callback(t, "github", cookie, state, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("link with no session: %d %s", rec.Code, rec.Body.String())
	}
	u, err := f.db.UserByEmail(context.Background(), "gone@example.com")
	if err != nil {
		t.Fatalf("look up: %v", err)
	}
	linked, err := f.db.IdentitiesOf(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(linked) != 0 {
		t.Fatal("a link was made without a session behind it")
	}
}

// Signing in as somebody else ends the previous session.
//
// Left in place, the old session outranks the new pending login at
// requireEnrolable — so enrolment would set a factor on the previous account
// and hand back a session for it.
func TestAProviderSignInEndsTheSessionAlreadyInTheBrowser(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "first@example.com")
	session, _, _ := f.enrol(t, "first@example.com")
	f.fakeFor("github", "gh-19", "second@example.com", "Second")

	// Start with no session, so the intent is a sign-in, then present the stale
	// session at the callback — a browser with one account signed in already.
	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state,
		map[string]string{sessionCookie: session})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}

	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("signing in as somebody else left the previous session in the browser")
	}
}

// Enrolment cannot spend a pending login belonging to another account.
//
// The direct form of the property above: even with both cookies present, the
// account whose factor is being enrolled and the account the pending login
// belongs to must be the same one.
func TestEnrolmentRefusesAPendingLoginFromAnotherAccount(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "one@example.com")
	session, _, _ := f.enrol(t, "one@example.com")

	// A second, factorless account with a pending login of its own.
	f.verifiedAccount(t, "two@example.com")
	login := f.postFrom(t, "198.51.100.77", "/api/auth/login",
		`{"email":"two@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)
	if pending == "" {
		t.Fatal("login set no pending cookie")
	}

	begin := f.getWithCookies(t, "/api/account/identities",
		map[string]string{sessionCookie: session})
	if begin.Code != http.StatusOK {
		t.Fatalf("sanity: the session does not work: %d", begin.Code)
	}

	// Both cookies. requireEnrolable prefers the session, so enrolment runs
	// against account one while the pending login belongs to account two.
	r := httptest.NewRequest(http.MethodPost, "/api/auth/2fa/enrol/begin",
		strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = f.nextIP() + ":1234"
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	r.AddCookie(&http.Cookie{Name: pendingCookie, Value: pending})
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("enrol begin: %d %s", rec.Code, rec.Body.String())
	}
	var started struct{ Secret string }
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode: %v", err)
	}

	r = httptest.NewRequest(http.MethodPost, "/api/auth/2fa/enrol/finish",
		strings.NewReader(`{"code":`+jsonString(codeAt(t, started.Secret, time.Now()))+`}`))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = f.nextIP() + ":1234"
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	r.AddCookie(&http.Cookie{Name: pendingCookie, Value: pending})
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	if rec.Code == http.StatusOK {
		t.Fatal("enrolment spent another account's pending login and issued a session")
	}
}

// Starting a sign-in is rate limited like every other unauthenticated route.
func TestTheOAuthStartIsRateLimited(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-20", "limited@example.com", "Limited")

	const ip = "198.51.100.90"
	seen := false
	for i := 0; i < limiterMax+2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/auth/github", nil)
		r.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("%d starts from one address were all accepted", limiterMax+2)
	}
}

// The callback returns the browser to the sign-in application.
//
// Cloud serves that application, so the redirect is unconditional and its
// target is relative, resolving against the origin that just set the pending
// cookie.
func TestTheCallbackReturnsToTheSignInApp(t *testing.T) {
	f := newFixture(t)
	f.fakeFor("github", "gh-21", "redirect@example.com", "Redirect")

	cookie, state := f.startOAuth(t, "github", "")
	rec := f.callback(t, "github", cookie, state, nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/signin?next=") {
		t.Fatalf("Location is %q, want a relative /signin path", loc)
	}
	// An absolute URL would be a second place the origin is written down, and a
	// stale one strands the sign-in.
	if strings.Contains(loc, "://") {
		t.Errorf("Location is absolute: %q", loc)
	}
	// Still a pending login and still no session — the answer changed, not the
	// thing being answered.
	if cookieFrom(rec, pendingCookie) == "" {
		t.Error("the redirect carried no pending login")
	}
	if cookieFrom(rec, sessionCookie) != "" {
		t.Error("the redirect carried a session")
	}
	if !strings.Contains(loc, "enrol") {
		t.Errorf("the redirect does not say what is next: %s", loc)
	}
}
