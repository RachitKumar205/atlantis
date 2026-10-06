package server

import (
	"context"
	"encoding/json"
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

// stepUp posts a code to the step-up route the way a console page does: JSON,
// from origin, carrying the Cloud session cookie when session is not empty.
func (f *fixture) stepUp(t *testing.T, org, origin, session, code string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/orgs/"+org+"/step-up",
		strings.NewReader(`{"code":`+jsonString(code)+`}`))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.RemoteAddr = f.nextIP() + ":1234"
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

// stepUpMember seeds acme with testConsole and one enrolled admin, returning
// their Cloud session, TOTP secret and user id.
func (f *fixture) stepUpMember(t *testing.T, email string) (session, secret, userID string) {
	t.Helper()
	ctx := context.Background()
	u := f.verifiedAccount(t, email)
	session, secret, _ = f.enrol(t, email)
	if err := f.db.CreateOrg(ctx, "acme", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(ctx, "acme", testConsole); err != nil {
		t.Fatalf("register console: %v", err)
	}
	if err := f.db.AddMember(ctx, u.ID, "acme", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return session, secret, u.ID
}

// refusalCode reads the machine-readable reason from a step-up refusal.
func refusalCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal %s: %v", rec.Body.String(), err)
	}
	return body.Code
}

// assertConsoleCanRead fails unless rec carries the CORS headers that let a
// page on testConsole read it.
func assertConsoleCanRead(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != testConsole {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, testConsole)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want true", got)
	}
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

// A console page from before step-up moved into the console still opens
// /authorize?prompt=reauth in a popup. It gets a page telling it to reload, not
// an ordinary assertion the popup would spend on a second session.
func TestAStaleStepUpPopupIsToldToReload(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "member@example.com", "acme", testConsole, identity.RoleAdmin)

	rec := f.authorize(t, session, "org=acme&prompt=reauth")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("stale popup: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("prompt=reauth still redirects with an assertion")
	}
	if !strings.Contains(rec.Body.String(), "reload the console") {
		t.Errorf("the page does not say to reload: %s", rec.Body.String())
	}

	// A popup already open submits its form instead.
	r := httptest.NewRequest(http.MethodPost, "/authorize/reauth",
		strings.NewReader("org=acme&code=123456"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = f.nextIP() + ":1234"
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	post := httptest.NewRecorder()
	f.srv.ServeHTTP(post, r)
	if post.Code != http.StatusBadRequest || !strings.Contains(post.Body.String(), "reload the console") {
		t.Errorf("the popup's form: %d %s", post.Code, post.Body.String())
	}
}

// A correct code from the organisation's console produces a step-up assertion
// for that console, and the console can read it.
func TestStepUpMintsAStepUpAssertionForTheConsole(t *testing.T) {
	f := newFixture(t)
	session, secret, userID := f.stepUpMember(t, "elevate@example.com")

	rec := f.stepUp(t, "acme", testConsole, session, codeAt(t, secret, nextWindow()))
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up: %d %s", rec.Code, rec.Body.String())
	}
	assertConsoleCanRead(t, rec)

	var body struct {
		Assertion string `json:"assertion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Assertion == "" {
		t.Fatalf("no assertion in %s (%v)", rec.Body.String(), err)
	}
	registered, private := claimsOf(t, body.Assertion)
	if !private.StepUp {
		t.Fatal("the assertion does not carry step_up, so the console will refuse it")
	}
	if registered.Subject != userID {
		t.Errorf("subject = %q, want %q", registered.Subject, userID)
	}
	if !registered.Audience.Contains(testConsole) {
		t.Errorf("audience = %v, want %s", registered.Audience, testConsole)
	}
}

// A wrong code is refused with a reason the console can show.
func TestStepUpRefusesAWrongCode(t *testing.T) {
	f := newFixture(t)
	session, _, _ := f.stepUpMember(t, "member@example.com")

	rec := f.stepUp(t, "acme", testConsole, session, "000000")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body.String())
	}
	if got := refusalCode(t, rec); got != stepUpCodeRejected {
		t.Errorf("code = %q, want %q", got, stepUpCodeRejected)
	}
	if strings.Contains(rec.Body.String(), "assertion") {
		t.Errorf("a wrong code answered with an assertion: %s", rec.Body.String())
	}
	assertConsoleCanRead(t, rec)
}

// A code is spent once. Without this, a code read over a shoulder buys a second
// elevation inside its ninety-second window.
func TestStepUpRefusesAReplayedCode(t *testing.T) {
	f := newFixture(t)
	session, secret, _ := f.stepUpMember(t, "member@example.com")
	code := codeAt(t, secret, nextWindow())

	if first := f.stepUp(t, "acme", testConsole, session, code); first.Code != http.StatusOK {
		t.Fatalf("first step-up: %d %s", first.Code, first.Body.String())
	}
	second := f.stepUp(t, "acme", testConsole, session, code)
	if second.Code == http.StatusOK {
		t.Fatal("the same code elevated twice")
	}
	if got := refusalCode(t, second); got != stepUpCodeRejected {
		t.Errorf("code = %q, want %q", got, stepUpCodeRejected)
	}
}

// Membership is read from the database on every call, not trusted from the
// path.
func TestStepUpRefusesANonMember(t *testing.T) {
	f := newFixture(t)
	session, secret, _ := f.stepUpMember(t, "member@example.com")
	if err := f.db.CreateOrg(context.Background(), "other", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(context.Background(), "other", testConsole); err != nil {
		t.Fatalf("register console: %v", err)
	}

	rec := f.stepUp(t, "other", testConsole, session, codeAt(t, secret, nextWindow()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-member: %d %s", rec.Code, rec.Body.String())
	}
	if got := refusalCode(t, rec); got != stepUpNotMember {
		t.Errorf("code = %q, want %q", got, stepUpNotMember)
	}
}

// Only the organisation's own console may obtain an assertion, and a refused
// origin spends nothing.
//
// An origin that is no registered console gets no CORS headers, so its page
// cannot read even the refusal. Another organisation's console is a registered
// origin and can read why it was refused, but receives no assertion.
//
// The last step proves the order. Were the factor checked before the origin, a
// page on another origin could spend the user's code even though it cannot
// read the answer.
func TestStepUpAnswersOnlyTheOrganisationsConsole(t *testing.T) {
	f := newFixture(t)
	session, secret, _ := f.stepUpMember(t, "member@example.com")
	const elsewhere = "https://elsewhere.console.example"
	if err := f.db.CreateOrg(context.Background(), "elsewhere", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(context.Background(), "elsewhere", elsewhere); err != nil {
		t.Fatalf("register console: %v", err)
	}
	code := codeAt(t, secret, nextWindow())

	for _, origin := range []string{
		"",                            // no Origin at all
		"null",                        // a sandboxed or privacy-stripped page
		"https://evil.example",        // another site
		"http://acme.console.example", // the right host, the wrong scheme
	} {
		rec := f.stepUp(t, "acme", origin, session, code)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Origin %q: %d %s", origin, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Origin %q got Access-Control-Allow-Origin %q", origin, got)
		}
	}

	rec := f.stepUp(t, "acme", elsewhere, session, code)
	if rec.Code != http.StatusForbidden {
		t.Errorf("another organisation's console: %d %s", rec.Code, rec.Body.String())
	}
	if got := refusalCode(t, rec); got != stepUpWrongConsole {
		t.Errorf("another organisation's console: code = %q, want %q", got, stepUpWrongConsole)
	}
	if strings.Contains(rec.Body.String(), `"assertion"`) {
		t.Errorf("another organisation's console received an assertion: %s", rec.Body.String())
	}

	if rec := f.stepUp(t, "acme", testConsole, session, code); rec.Code != http.StatusOK {
		t.Fatalf("the code was spent by a refused origin: %d %s", rec.Code, rec.Body.String())
	}
}

// An organisation that does not exist answers as one the user is not in, so
// the route does not list which organisation names exist.
func TestStepUpDoesNotRevealWhichOrganisationsExist(t *testing.T) {
	f := newFixture(t)
	session, secret, _ := f.stepUpMember(t, "member@example.com")
	if err := f.db.CreateOrg(context.Background(), "other", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(context.Background(), "other", testConsole); err != nil {
		t.Fatalf("register console: %v", err)
	}

	missing := f.stepUp(t, "nosuch", testConsole, session, codeAt(t, secret, nextWindow()))
	notIn := f.stepUp(t, "other", testConsole, session, codeAt(t, secret, nextWindow()))
	if missing.Code != notIn.Code || refusalCode(t, missing) != refusalCode(t, notIn) {
		t.Errorf("a missing organisation (%d %s) and one the user is not in (%d %s) answer differently",
			missing.Code, missing.Body.String(), notIn.Code, notIn.Body.String())
	}
	if got := refusalCode(t, missing); got != stepUpNotMember {
		t.Errorf("code = %q, want %q", got, stepUpNotMember)
	}
}

// A registered console URL and a browser's Origin header are compared as
// origins: case, a default port and a path do not make them differ, on either
// side.
func TestStepUpMatchesTheConsoleAsAnOrigin(t *testing.T) {
	// A fixture each: a TOTP step is spent once, and only the next one is
	// within the allowed skew.
	for _, origin := range []string{testConsole, "HTTPS://acme.CONSOLE.example:443"} {
		t.Run(origin, func(t *testing.T) {
			f := newFixture(t)
			session, secret, _ := f.stepUpMember(t, "member@example.com")
			if err := f.db.SetConsoleURL(context.Background(), "acme", "https://ACME.Console.Example:443/console"); err != nil {
				t.Fatalf("register console: %v", err)
			}
			rec := f.stepUp(t, "acme", origin, session, codeAt(t, secret, nextWindow()))
			if rec.Code != http.StatusOK {
				t.Fatalf("Origin %q: %d %s", origin, rec.Code, rec.Body.String())
			}
		})
	}
}

// A backup code works where an authenticator code does.
func TestStepUpAcceptsABackupCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.verifiedAccount(t, "backup@example.com")
	session, _, codes := f.enrol(t, "backup@example.com")
	if len(codes) == 0 {
		t.Fatal("enrolment returned no backup codes")
	}
	if err := f.db.CreateOrg(ctx, "acme", ""); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := f.db.SetConsoleURL(ctx, "acme", testConsole); err != nil {
		t.Fatalf("register console: %v", err)
	}
	if err := f.db.AddMember(ctx, u.ID, "acme", identity.RoleAdmin); err != nil {
		t.Fatalf("add member: %v", err)
	}

	rec := f.stepUp(t, "acme", testConsole, session, codes[0])
	if rec.Code != http.StatusOK {
		t.Fatalf("backup code: %d %s", rec.Code, rec.Body.String())
	}
	again := f.stepUp(t, "acme", testConsole, session, codes[0])
	if again.Code == http.StatusOK {
		t.Fatal("the same backup code elevated twice")
	}
}

// With no Cloud session the refusal says so, and the console can read it.
// Without the CORS headers on this answer the console sees a network error and
// cannot tell the user to sign in to Cloud again.
func TestStepUpNeedsACloudSession(t *testing.T) {
	f := newFixture(t)
	f.stepUpMember(t, "member@example.com")

	rec := f.stepUp(t, "acme", testConsole, "", "123456")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d %s", rec.Code, rec.Body.String())
	}
	if got := refusalCode(t, rec); got != stepUpSessionRequired {
		t.Errorf("code = %q, want %q", got, stepUpSessionRequired)
	}
	assertConsoleCanRead(t, rec)
}

// The code travels as JSON, which is what makes a browser send the preflight.
func TestStepUpWantsJSON(t *testing.T) {
	f := newFixture(t)
	session, _, _ := f.stepUpMember(t, "member@example.com")

	r := httptest.NewRequest(http.MethodPost, "/api/orgs/acme/step-up",
		strings.NewReader(`{"code":"123456"}`))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Origin", testConsole)
	r.RemoteAddr = f.nextIP() + ":1234"
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d %s", rec.Code, rec.Body.String())
	}
}

// The preflight admits the credentialed JSON POST. Without Allow-Origin and
// Allow-Credentials here, every browser blocks the request before it is sent.
func TestStepUpPreflightAdmitsTheConsole(t *testing.T) {
	f := newFixture(t)

	r := httptest.NewRequest(http.MethodOptions, "/api/orgs/acme/step-up", nil)
	r.Header.Set("Origin", testConsole)
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: %d %s", rec.Code, rec.Body.String())
	}
	assertConsoleCanRead(t, rec)
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, want POST", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(strings.ToLower(got), "content-type") {
		t.Errorf("Access-Control-Allow-Headers = %q, want Content-Type", got)
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
