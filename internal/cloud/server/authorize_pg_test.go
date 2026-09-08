package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/rachitkumar205/atlantis/internal/analytics"
	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// Handing a signed-in user to an organisation's console.
//
// The property under everything here is that the assertion's contents come from
// rows rather than from the request: the role from cloud.memberships, the
// destination and audience from cloud.orgs. There is no parameter that could
// say otherwise, and several of these tests exist to prove that adding one
// would not help an attacker.

const testConsole = "https://acme.console.example"

// allowedTestAlgorithms mirrors what a console accepts, so parsing here cannot
// succeed on a token a console would reject for its algorithm.
var allowedTestAlgorithms = []jose.SignatureAlgorithm{jose.ES256}

// postFormWithCookie sends a form-encoded POST carrying one cookie, which is
// what the reauth page's form does.
func (f *fixture) postFormWithCookie(t *testing.T, path string, form url.Values, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = f.nextIP() + ":1234"
	if value != "" {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

// member seeds an organisation with a registered console and one member,
// returning a Cloud session for them.
func (f *fixture) member(t *testing.T, email, org, consoleURL string, role identity.Role) string {
	t.Helper()
	ctx := context.Background()

	u := f.verifiedAccount(t, email)
	session, _, _ := f.enrol(t, email)

	if err := f.db.CreateOrg(ctx, org, ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if consoleURL != "" {
		if err := f.db.SetConsoleURL(ctx, org, consoleURL); err != nil {
			t.Fatalf("register console: %v", err)
		}
	}
	if err := f.db.AddMember(ctx, u.ID, org, role); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return session
}

// authorize follows /authorize with a session and returns the response.
func (f *fixture) authorize(t *testing.T, session, query string) *httptest.ResponseRecorder {
	t.Helper()
	return f.getWithCookies(t, "/authorize?"+query,
		map[string]string{sessionCookie: session})
}

// assertionFrom pulls the minted token out of a redirect's fragment.
func assertionFrom(t *testing.T, rec *httptest.ResponseRecorder) (token, destination string) {
	t.Helper()
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("no Location header; body was %s", rec.Body.String())
	}
	base, frag, ok := strings.Cut(loc, "#")
	if !ok {
		t.Fatalf("Location carries no fragment: %s", loc)
	}
	q, err := url.ParseQuery(frag)
	if err != nil {
		t.Fatalf("parse fragment: %v", err)
	}
	if q.Get("assertion") == "" {
		t.Fatalf("fragment carries no assertion: %s", loc)
	}
	return q.Get("assertion"), base
}

// claimsOf reads an assertion without verifying it.
//
// Unverified: what is under test is what Cloud put in the token.
// internal/console/cloudauth covers whether a console accepts it.
func claimsOf(t *testing.T, token string) (jwt.Claims, identity.Private) {
	t.Helper()
	parsed, err := jwt.ParseSigned(token, allowedTestAlgorithms)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var registered jwt.Claims
	var private identity.Private
	if err := parsed.UnsafeClaimsWithoutVerification(&registered, &private); err != nil {
		t.Fatalf("claims: %v", err)
	}
	return registered, private
}

// Membership decides, and the assertion carries the role from the row.
func TestAuthorizeMintsTheRoleFromTheMembershipRow(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "viewer@example.com", "acme", testConsole, identity.RoleViewer)

	rec := f.authorize(t, session, "org=acme")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}

	token, dest := assertionFrom(t, rec)
	if dest != testConsole+"/login" {
		t.Errorf("destination is %q, want the registered console", dest)
	}

	registered, private := claimsOf(t, token)
	// The row says viewer. Nothing in the request could have said admin,
	// because there is no role parameter — this asserts the value travelled
	// from the row rather than from a default.
	if private.Role != identity.RoleViewer {
		t.Errorf("role is %q, want viewer", private.Role)
	}
	if private.Org != "acme" {
		t.Errorf("org is %q", private.Org)
	}
	if private.Email != "viewer@example.com" {
		t.Errorf("email is %q", private.Email)
	}
	// The audience is the destination. One value, so a token cannot be
	// delivered somewhere it would not verify.
	if len(registered.Audience) != 1 || registered.Audience[0] != testConsole {
		t.Errorf("audience is %v, want %q", registered.Audience, testConsole)
	}
	// An ordinary sign-in is not a step-up.
	if private.StepUp {
		t.Error("an ordinary authorize claimed a second factor was presented")
	}
}

// No membership, no assertion.
func TestAuthorizeRefusesANonMember(t *testing.T) {
	f := newFixture(t)
	f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	// A different, fully signed-in account with no row for acme.
	f.verifiedAccount(t, "outsider@example.com")
	outsider, _, _ := f.enrol(t, "outsider@example.com")

	rec := f.authorize(t, outsider, "org=acme")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("a refusal still redirected somewhere")
	}
}

// An organisation that does not exist answers the same way as one the user is
// not in, so this route cannot be used to enumerate organisations.
func TestAuthorizeDoesNotRevealWhichOrganisationsExist(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	notAMember := f.authorize(t, session, "org=other")
	noSuchOrg := f.authorize(t, session, "org=does-not-exist")

	if notAMember.Code != noSuchOrg.Code {
		t.Errorf("statuses differ: %d and %d", notAMember.Code, noSuchOrg.Code)
	}
	if notAMember.Body.String() != noSuchOrg.Body.String() {
		t.Errorf("bodies differ:\n  %s\n  %s", notAMember.Body.String(), noSuchOrg.Body.String())
	}
}

// An organisation with no registered console cannot be authorized into.
func TestAuthorizeRefusesAnOrgWithNoConsole(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "unprovisioned", "", identity.RoleAdmin)

	rec := f.authorize(t, session, "org=unprovisioned")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("an org with no console still produced a redirect")
	}
}

// Signing in requires a Cloud session.
func TestAuthorizeNeedsASession(t *testing.T) {
	f := newFixture(t)
	f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.getWithCookies(t, "/authorize?org=acme", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("authorize with no session: %d %s", rec.Code, rec.Body.String())
	}
	// And a pending login is not a session here either.
	login := f.postFrom(t, "198.51.100.61", "/api/auth/login",
		`{"email":"member@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)
	rec = f.getWithCookies(t, "/authorize?org=acme", map[string]string{sessionCookie: pending})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a pending login authorized: %d", rec.Code)
	}
}

// A console parameter is ignored, not honoured.
//
// The destination comes from a row. Asserted directly, because a parameter that
// is ignored and one that is unimplemented look identical from outside, and
// only the first survives the parameter being implemented.
func TestAuthorizeIgnoresACallerSuppliedConsole(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	for _, hostile := range []string{
		"https://evil.example",
		testConsole + ".evil.example",
		"//evil.example",
	} {
		rec := f.authorize(t, session, "org=acme&console="+url.QueryEscape(hostile))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, testConsole+"/login#") {
			t.Errorf("console=%s produced Location %q", hostile, loc)
		}
		if strings.Contains(loc, "evil.example") {
			t.Errorf("a request parameter reached the redirect: %q", loc)
		}
	}
}

// The destination follows the row when the row moves.
func TestAuthorizeFollowsTheRegisteredConsole(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	if err := f.db.SetConsoleURL(context.Background(), "acme", "https://moved.example"); err != nil {
		t.Fatalf("move console: %v", err)
	}
	rec := f.authorize(t, session, "org=acme")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	token, dest := assertionFrom(t, rec)
	if dest != "https://moved.example/login" {
		t.Errorf("destination is %q", dest)
	}
	registered, _ := claimsOf(t, token)
	if len(registered.Audience) != 1 || registered.Audience[0] != "https://moved.example" {
		t.Errorf("audience is %v; it did not move with the destination", registered.Audience)
	}
}

// The assertion travels in the fragment, never the query string.
//
// A fragment is not sent to a server, so it stays out of the console's access
// log and out of the Referer of the next navigation. A query parameter would
// put a live credential in both.
func TestTheAssertionTravelsInTheFragment(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.authorize(t, session, "org=acme")
	loc := rec.Header().Get("Location")
	base, _, _ := strings.Cut(loc, "#")
	if strings.Contains(base, "assertion") || strings.Contains(base, "?") {
		t.Fatalf("the assertion is outside the fragment: %s", loc)
	}
}

// prompt=reauth does not redirect. It asks for a factor first.
func TestReauthDemandsAFactorEvenWithASession(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.authorize(t, session, "org=acme&prompt=reauth")
	if rec.Code != http.StatusOK {
		t.Fatalf("reauth: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("prompt=reauth redirected instead of asking for a code")
	}
	if !strings.Contains(rec.Body.String(), "authenticator") {
		t.Errorf("the page does not ask for a code: %s", rec.Body.String())
	}
}

// The reauth page's CSP permits the redirect the page exists to make.
//
// The form POSTs same-origin, so `form-action 'self'` looks correct and is
// not: the handler answers 303 to the CONSOLE's origin, and browsers enforce
// form-action across the whole redirect chain. With `'self'` alone the browser
// accepts the POST, lets the server mint the assertion, and then refuses to
// follow the redirect.
//
// The failure has no symptom. Nothing navigates, nothing errors, and the
// operator resends a code that is now spent and is told the code is wrong —
// the one explanation that is false. Found in a browser, because no test could
// see it: httptest does not enforce CSP, and the assertion below is the
// closest a test can get.
func TestTheReauthPageAllowsItsOwnRedirect(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "csp@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.authorize(t, session, "org=acme&prompt=reauth")
	if rec.Code != http.StatusOK {
		t.Fatalf("reauth: %d %s", rec.Code, rec.Body.String())
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the reauth page carries no policy at all")
	}
	// The console's origin, by name. Not a wildcard: it is this organisation's
	// console_url, the same value the assertion's audience is pinned to.
	if !strings.Contains(csp, "form-action 'self' "+testConsole) {
		t.Errorf("form-action does not permit the console:\n%s", csp)
	}
	// And nothing else was loosened while fixing that.
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the page stopped being script-free:\n%s", csp)
	}
	for _, banned := range []string{"'unsafe-inline'", "script-src", "*"} {
		if strings.Contains(csp, banned) {
			t.Errorf("the reauth policy now allows %s:\n%s", banned, csp)
		}
	}

	// The retry screen. serveReauthPage has two callers, the GET above and the
	// wrong-code re-render, and a fix applied to only the first passes
	// everything above while leaving the screen reached after a failed attempt
	// broken.
	retry := f.postFormWithCookie(t, "/authorize/reauth",
		url.Values{"org": {"acme"}, "code": {"000000"}}, sessionCookie, session)
	if retry.Code != http.StatusOK {
		t.Fatalf("the wrong-code re-render: %d %s", retry.Code, retry.Body.String())
	}
	if got := retry.Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("the retry screen carries a different policy:\n got  %q\n want %q", got, csp)
	}
}

// A correct code produces an assertion that says so.
func TestReauthMintsAStepUpAssertion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	u := f.verifiedAccount(t, "elevate@example.com")
	session, secret, _ := f.enrol(t, "elevate@example.com")
	if err := f.db.CreateOrg(ctx, "acme", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(ctx, "acme", testConsole); err != nil {
		t.Fatalf("register console: %v", err)
	}
	if err := f.db.AddMember(ctx, u.ID, "acme", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}

	code := codeAt(t, secret, nextWindow())
	rec := f.postFormWithCookie(t, "/authorize/reauth",
		url.Values{"org": {"acme"}, "code": {code}}, sessionCookie, session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reauth: %d %s", rec.Code, rec.Body.String())
	}

	token, _ := assertionFrom(t, rec)
	_, private := claimsOf(t, token)
	if !private.StepUp {
		t.Fatal("a reauth assertion does not carry step_up, so the console will refuse it")
	}
	// And the fragment tells the console page it is a popup handing back.
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "mode=reauth") {
		t.Errorf("the redirect does not mark itself as a step-up: %s", loc)
	}
}

// A wrong code is refused, and the page comes back rather than a dead end.
func TestReauthRefusesAWrongCode(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.postFormWithCookie(t, "/authorize/reauth",
		url.Values{"org": {"acme"}, "code": {"000000"}}, sessionCookie, session)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a wrong code produced an assertion")
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("a wrong code still redirected")
	}
	if !strings.Contains(rec.Body.String(), "not right") {
		t.Errorf("the page does not say the code was wrong: %s", rec.Body.String())
	}
}

// Membership is re-checked on the POST, not trusted from the form.
//
// The form was served to this user, but nothing obliges a POST to have come
// from it, and an org read out of a request body is a request parameter like
// any other.
func TestReauthRechecksMembershipOnThePost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	u := f.verifiedAccount(t, "member@example.com")
	session, secret, _ := f.enrol(t, "member@example.com")
	for _, org := range []string{"acme", "other"} {
		if err := f.db.CreateOrg(ctx, org, ""); err != nil {
			t.Fatalf("create org: %v", err)
		}
		if err := f.db.SetConsoleURL(ctx, org, testConsole); err != nil {
			t.Fatalf("register console: %v", err)
		}
	}
	// A member of acme only.
	if err := f.db.AddMember(ctx, u.ID, "acme", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}

	code := codeAt(t, secret, nextWindow())
	rec := f.postFormWithCookie(t, "/authorize/reauth",
		url.Values{"org": {"other"}, "code": {code}}, sessionCookie, session)
	if rec.Code == http.StatusSeeOther {
		t.Fatal("a correct code elevated into an organisation the user is not in")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// The reauth page needs a session of its own.
func TestReauthNeedsASession(t *testing.T) {
	f := newFixture(t)
	f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.postFormWithCookie(t, "/authorize/reauth",
		url.Values{"org": {"acme"}, "code": {"123456"}}, sessionCookie, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reauth with no session: %d %s", rec.Code, rec.Body.String())
	}
}

// Entering a console is an event, because for somebody who already holds a
// Cloud session it is the whole of "logging in" — no new session is created,
// so account.signed_in never fires.
func TestAuthorizeReportsEnteringTheConsole(t *testing.T) {
	f := newFixture(t)
	rec := &analytics.Recorder{}
	f.srv.events = rec

	session := f.member(t, "viewer@example.com", "acme", testConsole, identity.RoleViewer)
	if got := f.authorize(t, session, "org=acme"); got.Code != http.StatusSeeOther {
		t.Fatalf("authorize: %d %s", got.Code, got.Body.String())
	}

	events := rec.Named(analytics.EventConsoleAuthorized)
	if len(events) != 1 {
		t.Fatalf("got %d %s events, want 1", len(events), analytics.EventConsoleAuthorized)
	}
	e := events[0]
	if e.Org != "acme" {
		t.Errorf("org %q, want acme", e.Org)
	}
	if e.DistinctID == "" {
		t.Error("the event carries no actor")
	}
	if e.Props["role"] != identity.RoleViewer {
		t.Errorf("role %v, want viewer", e.Props["role"])
	}
	// The assertion itself is a credential and the email is the person.
	for _, v := range e.Props {
		if s, ok := v.(string); ok && strings.Contains(s, "@") {
			t.Errorf("an address reached the event: %v", e.Props)
		}
	}
}

// A refused authorize reports nothing, so the event counts entries rather than
// attempts.
func TestARefusedAuthorizeReportsNothing(t *testing.T) {
	f := newFixture(t)
	rec := &analytics.Recorder{}
	f.srv.events = rec

	session := f.member(t, "viewer@example.com", "acme", testConsole, identity.RoleViewer)
	if got := f.authorize(t, session, "org=someone-elses"); got.Code == http.StatusSeeOther {
		t.Fatal("a non-member was authorized")
	}

	if got := len(rec.Named(analytics.EventConsoleAuthorized)); got != 0 {
		t.Fatalf("got %d events for a refusal, want 0", got)
	}
}
