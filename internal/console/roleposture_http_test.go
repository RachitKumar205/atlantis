package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Import plan and apply are admin routes, and apply re-authenticates.
//
// Apply reaches AdoptBaseline through the console's OPERATOR grant and
// rewrites the caller's checkpoint. A viewer session that can reach it holds
// operator power through a browser cookie.

func TestImportPlanAndApplyRefuseAViewer(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "viewer@example.com", "viewer")

	for _, path := range []string{
		"/api/schema/imports/imp_x/plan",
		"/api/schema/imports/imp_x/apply",
	} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, "POST", path, "{}", token))
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s as a viewer got %d, want 403. Body: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestImportApplyRequiresSudo(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/schema/imports/imp_x/apply", "{}", token))

	if w.Code != http.StatusForbidden {
		t.Fatalf("apply without sudo got %d, want 403. Body: %s", w.Code, w.Body.String())
	}
	// The code is what the SPA acts on: it opens the re-auth dialog rather
	// than printing the message, the same shape as the approve route.
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the refusal is not JSON: %v. Body: %s", err, w.Body.String())
	}
	if got["code"] != "sudo_required" {
		t.Errorf("refusal code = %q, want sudo_required", got["code"])
	}
}

// Sandbox creation is held to developer or admin. Every other sandbox route
// resolves the pubID through the owner-scoped lookup, so a role that cannot
// boot owns nothing those routes can reach.

func TestSandboxBootAndForkRefuseAViewer(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "viewer@example.com", "viewer")

	for _, path := range []string{"/api/sandbox", "/api/sandbox/sbx_x/fork"} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, "POST", path, "{}", token))
		if w.Code != http.StatusForbidden {
			t.Errorf("POST %s as a viewer got %d, want 403. Body: %s", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "developer or admin") {
			t.Errorf("POST %s refusal does not name the roles that may: %s", path, w.Body.String())
		}
	}

	// The list stays readable: it is scoped to the caller's own sandboxes,
	// and a viewer owns none.
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/sandbox", "", token))
	if w.Code != http.StatusOK {
		t.Errorf("GET /api/sandbox as a viewer got %d, want 200. Body: %s", w.Code, w.Body.String())
	}
}

// The developer role: a viewer's reads plus sandbox creation, and nameable as
// an approver_role. Everything admin-gated stays refused.

func TestADeveloperBootsASandboxAndCannotReachAdminRoutes(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "dev@example.com", "developer")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/sandbox", "{}", token))
	if w.Code != http.StatusOK {
		t.Errorf("boot as developer got %d, want 200. Body: %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/callers"},
		{"POST", "/api/callers/somecaller/restore"},
		{"PUT", "/api/policy"},
		{"PUT", "/api/callers/somecaller/apply-policy"},
		{"POST", "/api/schema/imports/imp_x/plan"},
		{"POST", "/api/schema/imports/imp_x/apply"},
		{"POST", "/api/schema/rollback"},
	} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, tc.method, tc.path, "{}", token))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s as a developer got %d, want 403. Body: %s",
				tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestADeveloperApprovesWhenThePolicyNamesDeveloper(t *testing.T) {
	f := newConsoleFixture(t)

	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "PUT", "/api/policy",
		`{"entries":[{"change_class":"PLAN_CLASS_DESTRUCTIVE","require_approval":true,"approver_role":"developer"}]}`,
		admin))
	if w.Code != http.StatusOK {
		t.Fatalf("naming developer as the approver got %d: %s", w.Code, w.Body.String())
	}

	planID := f.pendingPlan(t)

	// The middleware and decide() both compare the asserted role to the
	// stored approver_role exactly, so the admin who set the rule is refused
	// by it — naming a role delegates the decision, not a copy of it.
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/approve", `{"reason":"agreed"}`, admin))
	if w.Code != http.StatusForbidden {
		t.Errorf("an admin approving a developer-approvable class got %d, want 403. Body: %s",
			w.Code, w.Body.String())
	}

	dev := f.signIn(t, "dev@example.com", "developer")
	f.elevate(t, dev)
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/approve", `{"reason":"agreed in review"}`, dev))
	if w.Code != http.StatusOK {
		t.Fatalf("a developer approving a developer-approvable class got %d: %s",
			w.Code, w.Body.String())
	}
}

// The apply-policy routes, end to end through the BFF: sudo gates the write,
// the write lands on the org server, and reads answer for any signed-in user.
func TestApplyPolicyRoundTripsThroughTheConsole(t *testing.T) {
	f := newConsoleFixture(t)
	admin := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "PUT",
		"/api/callers/atlantis-console/apply-policy", `{"apply_policy":"always_ask"}`, admin))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "sudo_required") {
		t.Fatalf("setting the tier without sudo got %d: %s", w.Code, w.Body.String())
	}

	f.elevate(t, admin)
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "PUT",
		"/api/callers/atlantis-console/apply-policy", `{"apply_policy":"always_ask"}`, admin))
	if w.Code != http.StatusOK {
		t.Fatalf("setting the tier got %d: %s", w.Code, w.Body.String())
	}

	viewer := f.signIn(t, "viewer@example.com", "viewer")
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET",
		"/api/callers/atlantis-console/apply-policy", "", viewer))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "always_ask") {
		t.Errorf("reading the tier back got %d: %s", w.Code, w.Body.String())
	}
	if n := f.auditCount(t, "apply_policy_set"); n != 1 {
		t.Errorf("%d apply_policy_set audit rows, want 1", n)
	}
}

func TestAViewerCannotBeNamedApprover(t *testing.T) {
	f := newConsoleFixture(t)
	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)

	for _, role := range []string{"viewer", "adminn"} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, "PUT", "/api/policy",
			`{"entries":[{"change_class":"PLAN_CLASS_DESTRUCTIVE","require_approval":true,"approver_role":"`+role+`"}]}`,
			admin))
		if w.Code == http.StatusOK {
			t.Errorf("approver_role=%q was accepted; that class is now one no session can decide", role)
		}
		if !strings.Contains(w.Body.String(), "cannot be an approver_role") {
			t.Errorf("the refusal of %q does not say what is wrong: %s", role, w.Body.String())
		}
	}
}

// The protection, freeze and override routes: admin + sudo to write, any
// signed-in user to read, and the override lands with its decided_via.
func TestProtectionAndFreezeRoutesRoundTrip(t *testing.T) {
	f := newConsoleFixture(t)
	admin := f.signIn(t, "admin@example.com", "admin")

	// Writes without sudo are refused.
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/api/protected-entities", `{"pattern":"conhttp.*","floor":"require_approval"}`},
		{"POST", "/api/freeze-windows", `{"starts_at":"2030-01-01T00:00:00Z","ends_at":"2030-01-02T00:00:00Z"}`},
	} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, tc.method, tc.path, tc.body, admin))
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "sudo_required") {
			t.Errorf("%s %s without sudo got %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	f.elevate(t, admin)
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "PUT", "/api/protected-entities",
		`{"pattern":"conhttp.Ledger","floor":"admin_only","reason":"holds money"}`, admin))
	if w.Code != http.StatusOK {
		t.Fatalf("putting a protection got %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/freeze-windows",
		`{"starts_at":"2030-01-01T00:00:00Z","ends_at":"2030-01-02T00:00:00Z","reason":"holiday"}`, admin))
	if w.Code != http.StatusOK {
		t.Fatalf("creating a freeze window got %d: %s", w.Code, w.Body.String())
	}

	// Reads answer for a viewer.
	viewer := f.signIn(t, "viewer@example.com", "viewer")
	for _, path := range []string{"/api/protected-entities", "/api/freeze-windows"} {
		w := httptest.NewRecorder()
		f.srv.handler.ServeHTTP(w, f.request(t, "GET", path, "", viewer))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s as a viewer got %d: %s", path, w.Code, w.Body.String())
		}
	}
	if n := f.auditCount(t, "protected_entity_put"); n != 1 {
		t.Errorf("%d protected_entity_put audit rows, want 1", n)
	}
	if n := f.auditCount(t, "freeze_window_created"); n != 1 {
		t.Errorf("%d freeze_window_created audit rows, want 1", n)
	}
}

func TestOverrideRouteApprovesPastTheGateAndIsAudited(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	admin := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, admin)

	// No reason, no override.
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/plans/"+planID+"/override", `{}`, admin))
	if w.Code != http.StatusBadRequest {
		t.Errorf("an override with no reason got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/plans/"+planID+"/override",
		`{"reason":"incident — reviewed by hand"}`, admin))
	if w.Code != http.StatusOK {
		t.Fatalf("the override got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"override"`) {
		t.Errorf("the response does not carry decided_via: %s", w.Body.String())
	}
	if n := f.auditCount(t, "override_schema_plan"); n != 1 {
		t.Errorf("%d override audit rows, want 1", n)
	}

	// A viewer cannot reach the route at all.
	viewer := f.signIn(t, "viewer@example.com", "viewer")
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/plans/"+planID+"/override",
		`{"reason":"x"}`, viewer))
	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer's override got %d: %s", w.Code, w.Body.String())
	}
}

// Rehearsing is developer-or-admin; the results list reads for anyone
// signed in (the server redacts row values from listings).
func TestRehearsalRoutesArePostured(t *testing.T) {
	f := newConsoleFixture(t)
	viewer := f.signIn(t, "viewer@example.com", "viewer")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/plans/pl_x/rehearse", "{}", viewer))
	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer's rehearse got %d, want 403: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/rehearsals", "", viewer))
	if w.Code != http.StatusOK {
		t.Errorf("GET /api/rehearsals as a viewer got %d: %s", w.Code, w.Body.String())
	}
}

func TestSandboxBootAndDestroyAreAudited(t *testing.T) {
	f := newConsoleFixture(t)
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST", "/api/sandbox", "{}", token))
	if w.Code != http.StatusOK {
		t.Fatalf("boot as admin got %d. Body: %s", w.Code, w.Body.String())
	}
	var boot sandboxBootResponse
	if err := json.Unmarshal(w.Body.Bytes(), &boot); err != nil {
		t.Fatalf("boot response: %v", err)
	}
	if got := f.auditCount(t, "sandbox_booted"); got != 1 {
		t.Errorf("sandbox_booted audit rows = %d, want 1", got)
	}

	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "DELETE", "/api/sandbox/"+boot.PubID, "", token))
	if w.Code != http.StatusNoContent {
		t.Fatalf("destroy got %d. Body: %s", w.Code, w.Body.String())
	}
	if got := f.auditCount(t, "sandbox_destroyed"); got != 1 {
		t.Errorf("sandbox_destroyed audit rows = %d, want 1", got)
	}
}
