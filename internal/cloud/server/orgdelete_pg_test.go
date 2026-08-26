package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// readyOrg creates an organisation and marks it serving.
//
// Ready, not pending: the delete route is guarded on `ready` in the store, so a
// fixture that stopped at creation would make every test here pass at the guard
// rather than at the thing under test.
func readyOrg(t *testing.T, f *fixture, session, name string) {
	t.Helper()
	rec := f.postJSON(t, "/api/orgs", session, f.srvOrigin(),
		`{"name":"`+name+`","display_name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s = %d: %s", name, rec.Code, rec.Body.String())
	}
	if err := f.db.MarkProvisioned(context.Background(), name); err != nil {
		t.Fatal(err)
	}
}

func decodeOrg(t *testing.T, rec *httptest.ResponseRecorder) orgResponse {
	t.Helper()
	var o orgResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return o
}

// The confirmation is checked on the server.
//
// A browser dialog defends against a misclick alone; the request is four lines
// of JavaScript to issue directly. Checked only in the client, a delete naming
// the wrong organisation succeeds.
func TestDeletingNeedsTheOrganisationName(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "deleter@example.com")
	readyOrg(t, f, session, "confirmme")

	for _, body := range []string{
		`{}`,
		`{"confirm":""}`,
		`{"confirm":"something-else"}`,
		`{"confirm":"confirmm"}`, // one character short
	} {
		rec := f.postJSON(t, "/api/orgs/confirmme/delete", session, f.srvOrigin(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("delete with %s = %d, want 400", body, rec.Code)
		}
	}

	// Still serving after every refusal.
	p, err := f.db.ProvisioningFor(context.Background(), "confirmme")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != store.StateReady {
		t.Fatalf("state is %q after four refused deletes, want ready", p.State)
	}

	rec := f.postJSON(t, "/api/orgs/confirmme/delete", session, f.srvOrigin(),
		`{"confirm":"confirmme"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", rec.Code, rec.Body.String())
	}
	o := decodeOrg(t, rec)
	if o.State != string(store.StateDeleted) {
		t.Fatalf("state is %q after delete, want deleted", o.State)
	}
	if o.PurgeAfter == "" {
		t.Error("no purge_after in the response: the screen has nothing to say " +
			"about how long the organisation stays restorable")
	}
}

func TestDeletingRequiresAnOriginAndASession(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "guards@example.com")
	readyOrg(t, f, session, "guarded")

	// No Origin. Cloud's cookie is SameSite=Lax rather than Strict, so this
	// header is the second answer to a cross-site POST and not a formality.
	if rec := f.postJSON(t, "/api/orgs/guarded/delete", session, "",
		`{"confirm":"guarded"}`); rec.Code != http.StatusForbidden {
		t.Errorf("delete with no Origin = %d, want 403", rec.Code)
	}
	// Another origin.
	if rec := f.postJSON(t, "/api/orgs/guarded/delete", session, "https://evil.test",
		`{"confirm":"guarded"}`); rec.Code != http.StatusForbidden {
		t.Errorf("delete from a foreign origin = %d, want 403", rec.Code)
	}
	// No session.
	if rec := f.postJSON(t, "/api/orgs/guarded/delete", "", f.srvOrigin(),
		`{"confirm":"guarded"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("delete with no session = %d, want 401", rec.Code)
	}

	p, err := f.db.ProvisioningFor(context.Background(), "guarded")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != store.StateReady {
		t.Fatalf("state is %q after three refused deletes, want ready", p.State)
	}
}

// A member who is not an admin is refused, and a stranger is told nothing.
func TestOnlyAnAdminMayDelete(t *testing.T) {
	f := newFixture(t)
	adminSession := f.signedIn(t, "admin@example.com")
	readyOrg(t, f, adminSession, "rolecheck")

	viewerSession := f.signedIn(t, "viewer@example.com")
	viewer, err := f.db.UserByEmail(context.Background(), "viewer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool().Exec(context.Background(),
		`INSERT INTO cloud.memberships (user_id, org, role) VALUES ($1, 'rolecheck', 'viewer')`,
		viewer.ID); err != nil {
		t.Fatal(err)
	}

	// A viewer is told why: they already know the organisation exists, so
	// naming the reason discloses nothing and saves them guessing.
	if rec := f.postJSON(t, "/api/orgs/rolecheck/delete", viewerSession, f.srvOrigin(),
		`{"confirm":"rolecheck"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's delete = %d, want 403", rec.Code)
	}

	// A stranger gets 404, not 403. The other way round would confirm which
	// organisations exist to anybody who asked.
	strangerSession := f.signedIn(t, "stranger@example.com")
	if rec := f.postJSON(t, "/api/orgs/rolecheck/delete", strangerSession, f.srvOrigin(),
		`{"confirm":"rolecheck"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a non-member's delete = %d, want 404 — 403 would confirm the "+
			"organisation exists", rec.Code)
	}
}

func TestRestoreReturnsAnOrganisationToService(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "restorer@example.com")
	readyOrg(t, f, session, "bringback")

	if rec := f.postJSON(t, "/api/orgs/bringback/delete", session, f.srvOrigin(),
		`{"confirm":"bringback"}`); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}

	rec := f.postJSON(t, "/api/orgs/bringback/restore", session, f.srvOrigin(), `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", rec.Code, rec.Body.String())
	}
	o := decodeOrg(t, rec)
	if o.State != string(store.StateReady) {
		t.Fatalf("state is %q after restore, want ready", o.State)
	}
	// The window is gone with it. A restored organisation still carrying a
	// purge_after is one the reaper takes the moment that date passes.
	if o.PurgeAfter != "" {
		t.Errorf("purge_after is still %q after a restore", o.PurgeAfter)
	}

	// Restoring twice is refused rather than silently accepted: the second call
	// has nothing to restore.
	if rec := f.postJSON(t, "/api/orgs/bringback/restore", session, f.srvOrigin(),
		`{}`); rec.Code != http.StatusNotFound {
		t.Errorf("restoring a live organisation = %d, want 404", rec.Code)
	}
}

// Deleting must not be a way to erase somebody else's organisation, and the
// name in the path is what decides which one is deleted.
func TestDeletingActsOnThePathNotTheBody(t *testing.T) {
	f := newFixture(t)
	session := f.signedIn(t, "twoorgs@example.com")
	readyOrg(t, f, session, "keeper")
	readyOrg(t, f, session, "goner")

	// A confirm naming the *other* organisation must not delete either: it does
	// not match the path, so it is a refusal rather than a redirection.
	if rec := f.postJSON(t, "/api/orgs/goner/delete", session, f.srvOrigin(),
		`{"confirm":"keeper"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete of goner confirming keeper = %d, want 400", rec.Code)
	}
	for _, name := range []string{"keeper", "goner"} {
		p, err := f.db.ProvisioningFor(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if p.State != store.StateReady {
			t.Errorf("%s is %q, want ready — a mismatched confirm deleted something", name, p.State)
		}
	}
}
