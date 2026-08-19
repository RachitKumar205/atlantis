package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The console's half of the organisation switcher.
//
// A switch is a full-page navigation: the console draws a link to Cloud, Cloud
// re-reads the membership and mints, and the browser comes back to /login
// carrying an assertion for the other organisation. Nothing in this package
// authorizes the move. What it does is carry the list, replace the session, and
// keep everything keyed to the organisation actually in use.
//
// Every failure here is quiet. A list that never arrives draws no switcher; a
// session that is not replaced leaves a second live row nobody can see; an
// ownership key missing its organisation half shows one organisation's
// sandboxes to another. None of them produces an error anywhere.

// switchTarget is one entry of /api/auth/me's `orgs`.
type switchTarget struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// meBody reads /api/auth/me for a session.
func (f *consoleFixture) meBody(t *testing.T, token string) (org string, orgs []switchTarget) {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/auth/me", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/auth/me: status %d, body %s", w.Code, w.Body.String())
	}
	var got struct {
		Org  string         `json:"org"`
		Orgs []switchTarget `json:"orgs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/auth/me: %v", err)
	}
	return got.Org, got.Orgs
}

// TestTheOrgListReachesTheBrowserFromTheSessionRow is the whole delivery path:
// claim → session row → /api/auth/me.
//
// The middle step is the one worth pinning. The console cannot ask Cloud what a
// user belongs to — step 3 severed that on purpose, so that Cloud being down is
// not a console being down — so if the list is not on the row, there is nowhere
// for it to come from and the switcher silently never appears.
func TestTheOrgListReachesTheBrowserFromTheSessionRow(t *testing.T) {
	f := newConsoleFixture(t)

	token := f.exchange(t,
		f.assertionWithOrgs(t, defaultOrg, "two@example.com", "admin",
			[]string{"acme", "globex"}), "")

	// On the row, because that is the only place a page load can read it from.
	var stored []string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT orgs FROM console.sessions WHERE token = $1`, token).Scan(&stored); err != nil {
		t.Fatalf("read the session's orgs: %v", err)
	}
	if len(stored) != 2 || stored[0] != "acme" || stored[1] != "globex" {
		t.Fatalf("session row orgs = %v, want [acme globex]", stored)
	}

	org, orgs := f.meBody(t, token)
	if org != "acme" {
		t.Fatalf("org = %q, want acme", org)
	}
	if len(orgs) != 2 {
		t.Fatalf("orgs = %+v, want two entries", orgs)
	}
	if orgs[0].Name != "acme" || orgs[1].Name != "globex" {
		t.Fatalf("orgs = %+v, want acme then globex", orgs)
	}

	// The current organisation is in the list. A switcher that omitted it would
	// have to say what it is showing some other way, and a list whose selected
	// item is missing is one the reader has to reason about.
	//
	// Every URL points at Cloud, never at a console. Cloud is what re-reads the
	// membership and knows where that organisation's console lives — which may
	// not be this deployment at all.
	for _, o := range orgs {
		if !strings.HasPrefix(o.URL, f.srv.cfg.CloudIssuer+"/authorize?org=") {
			t.Errorf("%s: url = %q, want a Cloud /authorize link", o.Name, o.URL)
		}
		if strings.HasPrefix(o.URL, f.audience) {
			t.Errorf("%s: url = %q points at a console, not at Cloud", o.Name, o.URL)
		}
		if !strings.HasSuffix(o.URL, "org="+o.Name) {
			t.Errorf("%s: url = %q does not name its own organisation", o.Name, o.URL)
		}
	}
}

// TestASessionWithNoOrgListDrawsNoSwitcher covers every session opened before
// migration 0006, and any assertion Cloud minted while it could not read
// memberships.
//
// `orgs` must be an empty array rather than null: an absent list and an empty
// one mean the same thing to the page, and `null` is one more shape for it to
// handle for no gain.
func TestASessionWithNoOrgListDrawsNoSwitcher(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "solo@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/auth/me", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/auth/me: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"orgs":[]`) {
		t.Fatalf("orgs is not an empty array: %s", w.Body.String())
	}
}

// TestASwitchReplacesTheSessionRatherThanAddingOne.
//
// Overwriting the cookie makes the old row unreachable, which is not the same
// as gone: it stays valid until it expires, and a session nobody can reach
// still counts everywhere sessions are counted — sign-out-all, "other
// sessions", any future quota. One browser, one row.
func TestASwitchReplacesTheSessionRatherThanAddingOne(t *testing.T) {
	f := newConsoleFixture(t)

	const (
		email   = "switcher@example.com"
		subject = "usr_switcher"
	)
	both := []string{"acme", "globex"}

	first := f.exchange(t, f.mintAs(t, subject, "acme", email, "admin", false, both), "")
	if n := f.sessionCount(t, subject); n != 1 {
		t.Fatalf("after sign-in: %d sessions, want 1", n)
	}

	// The switch, as the browser performs it: the old cookie is still being
	// sent when the new assertion is spent.
	second := f.exchange(t, f.mintAs(t, subject, "globex", email, "admin", false, both), first)
	if second == first {
		t.Fatal("the exchange reused the session token, so nothing was replaced")
	}
	if n := f.sessionCount(t, subject); n != 1 {
		t.Fatalf("after the switch: %d sessions, want 1", n)
	}

	// And the old token is not merely unreachable — it no longer authenticates.
	if code := f.meStatus(t, first); code != http.StatusUnauthorized {
		t.Fatalf("the replaced session still authenticates: /api/auth/me = %d", code)
	}
	if org, _ := f.meBody(t, second); org != "globex" {
		t.Fatalf("after the switch the session is in %q, want globex", org)
	}
}

// TestASecondBrowserIsNotSignedOutByAFirstOnesSwitch.
//
// The delete above is keyed to the cookie the request carried, not to the
// subject. Widening it to "every session for this person" would turn switching
// organisation on a laptop into being signed out on a phone — which is a
// perfectly plausible way to write it, produces no error, and would be noticed
// only as an intermittent complaint.
func TestASecondBrowserIsNotSignedOutByAFirstOnesSwitch(t *testing.T) {
	f := newConsoleFixture(t)

	const (
		email   = "twodevices@example.com"
		subject = "usr_twodevices"
	)
	both := []string{"acme", "globex"}

	laptop := f.exchange(t, f.mintAs(t, subject, "acme", email, "admin", false, both), "")
	phone := f.exchange(t, f.mintAs(t, subject, "acme", email, "admin", false, both), "")
	if n := f.sessionCount(t, subject); n != 2 {
		t.Fatalf("two browsers signed in: %d sessions, want 2", n)
	}

	f.exchange(t, f.mintAs(t, subject, "globex", email, "admin", false, both), laptop)

	if code := f.meStatus(t, phone); code != http.StatusOK {
		t.Fatalf("the other browser was signed out by a switch: /api/auth/me = %d", code)
	}
	if n := f.sessionCount(t, subject); n != 2 {
		t.Fatalf("after the switch: %d sessions, want 2 (the laptop's replacement and the phone's)", n)
	}
}

// sessionCount is how many live rows a subject holds.
func (f *consoleFixture) sessionCount(t *testing.T, subject string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM console.sessions WHERE subject = $1 AND expires_at > NOW()`,
		subject).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// TestSandboxesDoNotCrossTheSwitch.
//
// A sandbox is booted from one organisation's schema, so reaching one from a
// session in another organisation is reading that organisation's shape. Before
// the switcher this needed two sign-ins to reach; now it is one click, and the
// ownership key was the subject alone — which does not change across a switch.
//
// Three ways a sandbox can be reached, and the organisation has to hold on all
// three: listed, opened by id, and counted against the limit. The last is the
// quiet one — it does not expose anything, it just refuses a boot while naming
// a limit the person appears to be nowhere near.
func TestSandboxesDoNotCrossTheSwitch(t *testing.T) {
	f := newConsoleFixture(t)

	const (
		email   = "sandboxer@example.com"
		subject = "usr_sandboxer"
	)
	both := []string{"acme", "globex"}

	acme := f.exchange(t, f.mintAs(t, subject, defaultOrg, email, "admin", false, both), "")

	// A real sim sandbox, booted through the real endpoint against the
	// organisation's real atlantis. The fixture's limit is one.
	boot := f.post(t, "/api/sandbox", `{"backend":"sim"}`, acme)
	if boot.Code != http.StatusOK {
		t.Fatalf("boot: status %d, body %s", boot.Code, boot.Body.String())
	}
	var booted struct {
		PubID string `json:"pub_id"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &booted); err != nil {
		t.Fatalf("decode boot response: %v", err)
	}
	if booted.PubID == "" {
		t.Fatalf("boot returned no pub id: %s", boot.Body.String())
	}

	// The switch. Same subject, different organisation — which is exactly the
	// pair the old key could not tell apart.
	globex := f.exchange(t, f.mintAs(t, subject, "globex", email, "admin", false, both), acme)

	if got := f.sandboxIDs(t, globex); len(got) != 0 {
		t.Errorf("the other organisation lists %v", got)
	}

	// Not counted. globex has no atlantis registered in this fixture, so its
	// boot cannot succeed — but it must fail for the right reason, and 429 is
	// the wrong one.
	//
	// Before the destroy below, deliberately. A build where the organisation is
	// not part of the key answers that destroy with 204, and a sandbox that has
	// just been destroyed is not counted against anything — so checking the
	// count afterwards would agree with the broken build.
	if w := f.post(t, "/api/sandbox", `{"backend":"sim"}`, globex); w.Code == http.StatusTooManyRequests {
		t.Errorf("the other organisation is billed for it: %s", w.Body.String())
	}

	if w := f.delete(t, "/api/sandbox/"+booted.PubID, globex); w.Code != http.StatusNotFound {
		t.Errorf("the other organisation can reach it by id: status %d, body %s", w.Code, w.Body.String())
	}

	// The controls. Without these the assertions above pass on a console where
	// sandboxes simply do not work.
	back := f.exchange(t, f.mintAs(t, subject, defaultOrg, email, "admin", false, both), globex)
	if got := f.sandboxIDs(t, back); len(got) != 1 || got[0] != booted.PubID {
		t.Errorf("after switching back the sandbox is %v, want [%s]", got, booted.PubID)
	}
	if w := f.post(t, "/api/sandbox", `{"backend":"sim"}`, back); w.Code != http.StatusTooManyRequests {
		t.Errorf("the limit stopped applying inside the organisation: status %d, body %s",
			w.Code, w.Body.String())
	}
}

// sandboxIDs lists the pub ids a session can see.
func (f *consoleFixture) sandboxIDs(t *testing.T, token string) []string {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodGet, "/api/sandbox", "", token))
	if w.Code != http.StatusOK {
		t.Fatalf("/api/sandbox: status %d, body %s", w.Code, w.Body.String())
	}
	var got struct {
		Sandboxes []struct {
			PubID string `json:"pub_id"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/sandbox: %v", err)
	}
	out := make([]string, 0, len(got.Sandboxes))
	for _, s := range got.Sandboxes {
		out = append(out, s.PubID)
	}
	return out
}

func (f *consoleFixture) delete(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, http.MethodDelete, path, "", token))
	return w
}

// TestSandboxOwnershipIsSubjectAndOrg is the same property at the layer, where
// the comparison actually lives.
//
// The HTTP test above proves the handlers pass the organisation through. This
// one proves the layer uses it, and it covers the pairing the handlers cannot
// reach: two different people, one in each organisation, where dropping either
// half of the key still looks correct from one direction.
func TestSandboxOwnershipIsSubjectAndOrg(t *testing.T) {
	// No runtimes are registered, so nothing needs closing: these metas exist
	// only to be matched against, which is the whole of what the key does.
	l := newSandboxLayer(4, time.Hour)

	mine := &sandboxMeta{pubID: "p1", ownerSubject: "usr_a", ownerOrg: "acme"}
	sameSubjectOtherOrg := &sandboxMeta{pubID: "p2", ownerSubject: "usr_a", ownerOrg: "globex"}
	otherSubjectSameOrg := &sandboxMeta{pubID: "p3", ownerSubject: "usr_b", ownerOrg: "acme"}
	for _, m := range []*sandboxMeta{mine, sameSubjectOtherOrg, otherSubjectSameOrg} {
		l.register(m)
	}

	for _, tc := range []struct {
		name     string
		pubID    string
		subject  string
		org      string
		wantSeen bool
	}{
		{"its own owner", "p1", "usr_a", "acme", true},
		{"the same person in another organisation", "p1", "usr_a", "globex", false},
		{"another person in the same organisation", "p1", "usr_b", "acme", false},
		{"the other organisation's own", "p2", "usr_a", "globex", true},
	} {
		if _, ok := l.lookup(tc.pubID, tc.subject, tc.org); ok != tc.wantSeen {
			t.Errorf("lookup(%s) by %s: seen = %v, want %v", tc.name, tc.subject, ok, tc.wantSeen)
		}
	}

	for _, tc := range []struct {
		subject string
		org     string
		want    []string
	}{
		{"usr_a", "acme", []string{"p1"}},
		{"usr_a", "globex", []string{"p2"}},
		{"usr_b", "acme", []string{"p3"}},
		{"usr_b", "globex", nil},
	} {
		got := l.listForUser(tc.subject, tc.org)
		if len(got) != len(tc.want) {
			t.Fatalf("listForUser(%s, %s) = %d entries, want %d", tc.subject, tc.org, len(got), len(tc.want))
		}
		for i := range got {
			if got[i].pubID != tc.want[i] {
				t.Errorf("listForUser(%s, %s)[%d] = %s, want %s",
					tc.subject, tc.org, i, got[i].pubID, tc.want[i])
			}
		}
		if n := l.countForUser(tc.subject, tc.org); n != len(tc.want) {
			t.Errorf("countForUser(%s, %s) = %d, want %d", tc.subject, tc.org, n, len(tc.want))
		}
	}
}
