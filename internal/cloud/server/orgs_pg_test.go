package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
)

// The signed-in half of Cloud's API.

// decodeMe reads /api/account/me's body.
func decodeMe(t *testing.T, rec *httptest.ResponseRecorder) meResponse {
	t.Helper()
	var out meResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func TestMeNeedsASession(t *testing.T) {
	f := newFixture(t)
	rec := f.get(t, "/api/account/me")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/account/me without a session = %d, want 401", rec.Code)
	}
}

// The account and its organisations, with enough to render a screen.
func TestMeReportsTheAccountAndItsOrganisations(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "me@example.com", "me-org", "http://console.test", identity.RoleAdmin)

	rec := f.getWithCookies(t, "/api/account/me", map[string]string{sessionCookie: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/account/me = %d: %s", rec.Code, rec.Body.String())
	}
	me := decodeMe(t, rec)

	if me.Email != "me@example.com" {
		t.Errorf("email = %q", me.Email)
	}
	if len(me.Orgs) != 1 {
		t.Fatalf("orgs = %d, want 1", len(me.Orgs))
	}
	o := me.Orgs[0]
	if o.Name != "me-org" || o.Role != string(identity.RoleAdmin) {
		t.Errorf("org = %+v", o)
	}
	// The link is built here, and it is always /authorize — never a console
	// directly, because the membership re-read is the gate.
	if !strings.Contains(o.URL, "/authorize?org=me-org") {
		t.Errorf("url = %q, want Cloud's /authorize", o.URL)
	}
	if strings.Contains(o.URL, "console.test") {
		t.Errorf("url = %q links straight to a console, skipping the membership check", o.URL)
	}
}

// An organisation with no console yet gets no link.
//
// /authorize answers its own 503 for one, so a link would reliably fail. The
// screen renders the state instead.
func TestAnUnprovisionedOrganisationHasNoLink(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "nolink@example.com", "nolink-org", "", identity.RoleAdmin)

	rec := f.getWithCookies(t, "/api/account/me", map[string]string{sessionCookie: session})
	me := decodeMe(t, rec)
	if len(me.Orgs) != 1 {
		t.Fatalf("orgs = %d", len(me.Orgs))
	}
	if me.Orgs[0].URL != "" {
		t.Errorf("url = %q for an organisation with no console", me.Orgs[0].URL)
	}
}

// The last provisioning error never reaches the browser.
//
// It is written for an operator and carries image references, cluster
// hostnames and API paths — one from the local walkthrough contained the whole
// Kubernetes API server URL, including the field manager and the namespace.
func TestTheProvisioningErrorIsNotPublished(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	session := f.member(t, "err@example.com", "err-org", "", identity.RoleAdmin)

	// The shape a real failure takes.
	const leak = `Patch "https://atl-dev.test:6443/api/v1/namespaces/org-err-org?fieldManager=atlantis-provisioner": tls: failed to verify certificate`
	if _, err := f.db.Pool().Exec(ctx, `
		INSERT INTO cloud.org_provisioning (org, state, last_error, attempts)
		VALUES ($1, 'failed', $2, 3)
		ON CONFLICT (org) DO UPDATE SET state = 'failed', last_error = EXCLUDED.last_error, attempts = 3
	`, "err-org", leak); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := f.getWithCookies(t, "/api/account/me", map[string]string{sessionCookie: session})
	body := rec.Body.String()

	for _, secret := range []string{"atl-dev.test", "6443", "fieldManager", "namespaces", "tls:"} {
		if strings.Contains(body, secret) {
			t.Errorf("the response carries %q from the operator-facing error:\n%s", secret, body)
		}
	}
	// The state and the attempt count are what the screen legitimately needs.
	me := decodeMe(t, rec)
	if len(me.Orgs) != 1 || me.Orgs[0].State != "failed" || me.Orgs[0].Attempts != 3 {
		t.Errorf("the screen cannot tell it failed: %+v", me.Orgs)
	}
}

// Reading one organisation, for a screen waiting on it.
func TestGetOrgReportsOneOrganisation(t *testing.T) {
	f := newFixture(t)
	session := f.member(t, "one@example.com", "one-org", "http://console.test", identity.RoleAdmin)

	rec := f.getWithCookies(t, "/api/orgs/one-org", map[string]string{sessionCookie: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/orgs/one-org = %d: %s", rec.Code, rec.Body.String())
	}
	var o orgResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if o.Name != "one-org" {
		t.Errorf("name = %q", o.Name)
	}
}

// An organisation you are not in is indistinguishable from one that is not
// there.
//
// Otherwise the route reports which organisations exist to anybody with an
// account, one guess at a time.
func TestAnOutsiderCannotTellAnOrganisationExists(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Somebody else's organisation.
	f.member(t, "insider@example.com", "secret-org", "http://console.test", identity.RoleAdmin)

	outsider := f.verifiedAccount(t, "outsider@example.com")
	session, _, _ := f.enrol(t, "outsider@example.com")
	_ = outsider
	_ = ctx

	real := f.getWithCookies(t, "/api/orgs/secret-org", map[string]string{sessionCookie: session})
	fake := f.getWithCookies(t, "/api/orgs/no-such-org", map[string]string{sessionCookie: session})

	if real.Code != http.StatusNotFound || fake.Code != http.StatusNotFound {
		t.Fatalf("codes = %d and %d, want 404 for both", real.Code, fake.Code)
	}
	if real.Body.String() != fake.Body.String() {
		t.Errorf("the two answers differ, revealing which organisation exists:\n %s\n %s",
			real.Body.String(), fake.Body.String())
	}
}

// srvOrigin is the origin the fixture's server considers its own.
//
// Matches the fixture's PublicURL, which is what sameOrigin compares against —
// deliberately, rather than the request Host, so that Vite's proxy does not
// refuse every write in development.
func (f *fixture) srvOrigin() string { return "https://cloud.test" }

// postJSON sends a JSON body with a session cookie and an Origin.
//
// Origin is a parameter because it is the thing under test in several of these:
// the check is required on /api/*, and "required" is only meaningful if a
// missing header is refused.
func (f *fixture) postJSON(t *testing.T, path, session, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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

// signedIn returns a session for a verified, enrolled account.
func (f *fixture) signedIn(t *testing.T, email string) string {
	t.Helper()
	f.verifiedAccount(t, email)
	session, _, _ := f.enrol(t, email)
	return session
}

// Creating an organisation, which is the whole point of P4.
func TestCreatingAnOrganisationQueuesIt(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "creator@example.com")

	rec := f.postJSON(t, "/api/orgs", session, f.srvOrigin(),
		`{"name":"brand-new","display_name":"Brand New"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var o orgResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if o.Name != "brand-new" || o.State != "pending" || !o.CreatedByMe {
		t.Errorf("response = %+v", o)
	}

	// And it is queued, which is what makes the provisioner pick it up.
	p, err := f.db.ProvisioningFor(context.Background(), "brand-new")
	if err != nil {
		t.Fatalf("not queued: %v", err)
	}
	if p.State != "pending" {
		t.Errorf("queue state = %q", p.State)
	}
}

// Posting a name somebody else has grants nothing, and says so uniformly.
func TestCreatingATakenNameIsRefusedOverHTTP(t *testing.T) {
	f := newFixture(t)
	owner := f.signedIn(t, "owner-http@example.com")
	if rec := f.postJSON(t, "/api/orgs", owner, f.srvOrigin(),
		`{"name":"contested","display_name":"Original"}`); rec.Code != http.StatusCreated {
		t.Fatalf("owner create = %d: %s", rec.Code, rec.Body.String())
	}

	other := f.signedIn(t, "other-http@example.com")
	rec := f.postJSON(t, "/api/orgs", other, f.srvOrigin(),
		`{"name":"contested","display_name":"Hijacked"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("taken name = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	// A reserved name gets the identical answer: to whoever is typing, taken
	// and reserved are the same fact with the same remedy.
	reserved := f.postJSON(t, "/api/orgs", other, f.srvOrigin(), `{"name":"admin"}`)
	if reserved.Code == http.StatusConflict && reserved.Body.String() != rec.Body.String() {
		t.Errorf("a reserved name answers differently from a taken one:\n %s\n %s",
			reserved.Body.String(), rec.Body.String())
	}
}

// A state-changing route refuses a request with no Origin.
//
// The console's version of this check allows it and leans on SameSite=Strict.
// Cloud's cookie is Lax, so it cannot.
func TestCreateRefusesAMissingOrigin(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "noorigin@example.com")

	rec := f.postJSON(t, "/api/orgs", session, "", `{"name":"no-origin-org"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("create with no Origin = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRefusesAForeignOrigin(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "evil@example.com")

	rec := f.postJSON(t, "/api/orgs", session, "https://evil.example", `{"name":"evil-org"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("create from a foreign origin = %d, want 403", rec.Code)
	}
}

// The two form posts still work without an Origin.
//
// This is the half an earlier design would have broken. Referrer-Policy is
// no-referrer on every response, so a form navigation sends `Origin: null` —
// and POST /reset is reached from a mail client exactly that way.
func TestTheFormPostsDoNotRequireAnOrigin(t *testing.T) {
	f := newFixture(t)

	r := httptest.NewRequest(http.MethodPost, "/reset",
		strings.NewReader("token=nonsense&password=irrelevant"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = f.nextIP() + ":1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	// The token is nonsense, so this refuses — but it must not refuse for want
	// of an Origin header, which is the failure being guarded against.
	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "Origin") {
		t.Errorf("POST /reset was refused for a missing Origin: %s", rec.Body.String())
	}
}

// A bad name is refused with the rule, not a constraint.
func TestCreateRefusesABadNameWithAReason(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "badname@example.com")

	rec := f.postJSON(t, "/api/orgs", session, f.srvOrigin(), `{"name":"Bad Name"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "constraint") {
		t.Errorf("the refusal is a database error: %s", rec.Body.String())
	}
}

// Creating requires a session.
func TestCreateNeedsASession(t *testing.T) {
	f := newFixture(t)
	rec := f.postJSON(t, "/api/orgs", "", f.srvOrigin(), `{"name":"anon-org"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("create with no session = %d, want 401", rec.Code)
	}
}

// Creating writes the first human-actor row in cloud.audit_log.
func TestCreatingAnOrganisationIsAudited(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "audited@example.com")

	if rec := f.postJSON(t, "/api/orgs", session, f.srvOrigin(),
		`{"name":"audited-org"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}

	entries, err := f.db.AuditFor(context.Background(), "audited-org", 10)
	if err != nil {
		t.Fatalf("AuditFor: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("creating an organisation wrote no audit row")
	}
	var found bool
	for _, e := range entries {
		if e.Action == "org.created" {
			found = true
			if e.ActorEmail != "audited@example.com" {
				t.Errorf("actor email = %q, want the address that acted", e.ActorEmail)
			}
		}
	}
	if !found {
		t.Errorf("no org.created row: %+v", entries)
	}
}
