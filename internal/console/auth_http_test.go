package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
)

// post sends a JSON body to path with the given session cookie.
func (f *consoleFixture) post(t *testing.T, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodPost, path, body, token))
	return w
}

func exchangeBody(assertion string) string {
	return fmt.Sprintf(`{"assertion":%q}`, assertion)
}

// TestExchangeOpensASession is the whole sign-in path, end to end: Cloud mints,
// the console verifies against the published JWKS, and a session cookie comes
// back that later requests are accepted with.
func TestExchangeOpensASession(t *testing.T) {
	f := newConsoleFixture(t)

	w := f.post(t, "/api/auth/exchange", exchangeBody(f.assertion(t, "rachit@example.com", "admin")), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}

	var token string
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookieName {
			token = c.Value
			if !c.HttpOnly {
				t.Error("session cookie is readable from JavaScript")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("session cookie SameSite = %v, want Strict", c.SameSite)
			}
		}
	}
	if token == "" {
		t.Fatal("no session cookie was set")
	}

	// And the session carries the asserted identity through to the API.
	me := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(me, f.request(t, http.MethodGet, "/api/auth/me", "", token))
	if me.Code != http.StatusOK {
		t.Fatalf("/api/auth/me status = %d, body %s", me.Code, me.Body.String())
	}

	var got struct {
		Subject string `json:"subject"`
		Org     string `json:"org"`
		Email   string `json:"email"`
		Role    string `json:"role"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/auth/me: %v", err)
	}
	if want := subjectFor(defaultOrg, "rachit@example.com"); got.Subject != want {
		t.Errorf("subject = %q, want %q", got.Subject, subjectFor(defaultOrg, "rachit@example.com"))
	}
	if got.Org != "acme" {
		t.Errorf("org = %q", got.Org)
	}
	if got.Email != "rachit@example.com" || got.Role != "admin" || got.Name != "Test User" {
		t.Errorf("identity = %+v", got)
	}
}

// TestAssertionCannotBeExchangedTwice is the property single-use exists for.
//
// The assertion travels through a browser, so a copy captured in flight must
// be worth nothing once the legitimate request has landed.
func TestAssertionCannotBeExchangedTwice(t *testing.T) {
	f := newConsoleFixture(t)
	assertion := f.assertion(t, "replay@example.com", "admin")

	first := f.post(t, "/api/auth/exchange", exchangeBody(assertion), "")
	if first.Code != http.StatusOK {
		t.Fatalf("first exchange: status %d, body %s", first.Code, first.Body.String())
	}

	second := f.post(t, "/api/auth/exchange", exchangeBody(assertion), "")
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("a replayed assertion opened a second session: status %d, body %s",
			second.Code, second.Body.String())
	}
	for _, c := range second.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Error("the replay was refused but still set a session cookie")
		}
	}
}

func TestExchangeRefusesForeignAssertions(t *testing.T) {
	f := newConsoleFixture(t)

	// An issuer this console does not trust, with its own key. Cryptographically
	// impeccable and still not admissible.
	otherKey, err := issuer.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	otherCloud, err := issuer.New("https://not-cloud.example.com", otherKey)
	if err != nil {
		t.Fatalf("issuer.New: %v", err)
	}
	foreign, err := otherCloud.Mint(issuer.Grant{
		Subject: "usr_intruder", Org: "acme", Role: identity.RoleAdmin,
		Email: "intruder@example.com", Audience: f.audience,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// An assertion from the right issuer, but addressed to another console.
	// One stack per organisation, so this is the boundary between them.
	wrongAudience, err := f.iss.Mint(issuer.Grant{
		Subject: "usr_elsewhere", Org: "other", Role: identity.RoleAdmin,
		Email: "elsewhere@example.com", Audience: "https://other.console.test",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	for _, tc := range []struct {
		name      string
		assertion string
	}{
		{"issuer this console does not trust", foreign},
		{"minted for another organisation's console", wrongAudience},
		{"not a token at all", "garbage"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.post(t, "/api/auth/exchange", exchangeBody(tc.assertion), "")
			if w.Code == http.StatusOK {
				t.Fatalf("accepted: %s", w.Body.String())
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == sessionCookieName && c.Value != "" {
					t.Error("refused the assertion but set a session cookie anyway")
				}
			}
		})
	}
}

// TestSudoNeedsAFreshAssertion covers the step-up control.
//
// Sudo exists so that holding a session cookie is not enough to run a
// destructive action. With identity at Cloud, proving yourself again means
// going back to Cloud — so the assertion already spent on sign-in must not
// work here, or the control degrades into a button that always succeeds while
// looking exactly like a working one.
func TestSudoNeedsAFreshAssertion(t *testing.T) {
	f := newConsoleFixture(t)

	signIn := f.assertion(t, "sudo@example.com", "admin")
	ex := f.post(t, "/api/auth/exchange", exchangeBody(signIn), "")
	if ex.Code != http.StatusOK {
		t.Fatalf("exchange: status %d, body %s", ex.Code, ex.Body.String())
	}
	var token string
	for _, c := range ex.Result().Cookies() {
		if c.Name == sessionCookieName {
			token = c.Value
		}
	}

	// The sign-in assertion has been spent. Replaying it must not elevate.
	replay := f.post(t, "/api/auth/sudo", exchangeBody(signIn), token)
	if replay.Code == http.StatusOK {
		t.Fatal("the assertion already used to sign in also granted sudo")
	}

	// A fresh one does.
	fresh := f.post(t, "/api/auth/sudo", exchangeBody(f.assertion(t, "sudo@example.com", "admin")), token)
	if fresh.Code != http.StatusOK {
		t.Fatalf("a fresh assertion did not grant sudo: status %d, body %s",
			fresh.Code, fresh.Body.String())
	}
}

// TestSudoRefusesAnAssertionForSomebodyElse: without the subject check, anyone
// holding a valid assertion of their own could elevate a session belonging to
// a different user.
func TestSudoRefusesAnAssertionForSomebodyElse(t *testing.T) {
	f := newConsoleFixture(t)

	token := f.signIn(t, "owner@example.com", "admin")
	other := f.assertion(t, "someone-else@example.com", "admin")

	w := f.post(t, "/api/auth/sudo", exchangeBody(other), token)
	if w.Code == http.StatusOK {
		t.Fatal("one user's assertion elevated another user's session")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// TestAuditLogNamesAssertionAuthenticatedActors is the reason this step was
// sequenced before the org work.
//
// listAuditLog used to INNER JOIN console.users. With identity at Cloud there
// is no row there for anyone, so every audited action would have been written
// and then omitted from the listing — a log that looks healthy and empty
// rather than broken.
func TestAuditLogNamesAssertionAuthenticatedActors(t *testing.T) {
	f := newConsoleFixture(t)

	token := f.signIn(t, "auditor@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/audit?limit=50", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("audit listing: status %d, body %s", w.Code, w.Body.String())
	}

	var got struct {
		Entries []struct {
			Actor      string `json:"actor"`
			ActorEmail string `json:"actor_email"`
			Action     string `json:"action"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode audit listing: %v", err)
	}

	var found bool
	for _, e := range got.Entries {
		if e.Action != "signed_in" {
			continue
		}
		found = true
		if want := subjectFor(defaultOrg, "auditor@example.com"); e.Actor != want {
			t.Errorf("actor = %q, want the Cloud subject %q", e.Actor, subjectFor(defaultOrg, "auditor@example.com"))
		}
		if e.ActorEmail != "auditor@example.com" {
			t.Errorf("actor_email = %q", e.ActorEmail)
		}
	}
	if !found {
		t.Fatalf("the sign-in is absent from the audit listing (%d entries); "+
			"this is exactly the silent gap the inner join used to create", len(got.Entries))
	}
}

// TestAuditListingJoinsNothing is the sharper half of the test above.
//
// That one signs in and looks for its own entry — so it passes even if the
// listing joins the actor to some other table, because the actor it looks for
// has a row in every table there is. This one records an action by an actor
// with no session, no organisation membership here, and nothing else to match
// on: exactly what a pre-migration `local:N` row looks like, and what any
// action by someone since removed looks like.
//
// A join of any kind drops it, and the log would report fewer actions than
// were taken — while looking, from the page, entirely healthy.
func TestAuditListingJoinsNothing(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "reader@example.com", "admin")

	// Written straight through the store, in the same organisation the reader
	// is in: no session, no sign-in, nothing for a join to find.
	f.srv.db.forOrg(defaultOrg).logAction(context.Background(),
		"local:4242", "departed@example.com", "approve_plan", map[string]any{"version": 7})

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/audit?limit=50", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("audit listing: status %d, body %s", w.Code, w.Body.String())
	}

	var got struct {
		Entries []struct {
			Actor      string `json:"actor"`
			ActorEmail string `json:"actor_email"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode audit listing: %v", err)
	}

	for _, e := range got.Entries {
		if e.Actor == "local:4242" {
			if e.ActorEmail != "departed@example.com" {
				t.Errorf("actor_email = %q, want the address recorded on the row", e.ActorEmail)
			}
			return
		}
	}
	t.Fatalf("an action by an actor with no session is missing from the listing "+
		"(%d entries); the listing is joining something", len(got.Entries))
}
