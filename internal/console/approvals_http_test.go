package console

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The four gates on the approve route, each asserted by removing exactly one
// of the things a legitimate request carries.
//
// approve is wrapped as auth → requirePolicyRole → csrf → requireSudo. Order
// matters and nothing until now checked any of it. These are written as
// "the same request, minus one thing" so a failure names which layer stopped
// caring rather than reporting that some request was refused.

func TestApproveNeedsARoleTheChangePolicyNames(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	token := f.signIn(t, "viewer@example.com", "viewer")
	f.elevate(t, token)

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/approve", `{"reason":"looks fine"}`, token))

	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer approving a change that needs admin got %d, want 403. "+
			"Body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin") {
		t.Errorf("the refusal does not name the role required, so the viewer cannot "+
			"tell who to ask: %s", w.Body.String())
	}
	if n := f.auditCount(t, "approve_schema_plan"); n != 0 {
		t.Errorf("%d approval audit rows written for a refused request", n)
	}
}

func TestApproveNeedsSudo(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	// Correct role, correct origin, session valid — everything except the
	// password re-entry.
	token := f.signIn(t, "admin@example.com", "admin")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/approve", `{"reason":"agreed"}`, token))

	if w.Code != http.StatusForbidden {
		t.Errorf("an admin approved without re-authenticating: got %d, want 403. "+
			"A stolen session cookie alone would then be enough to release production "+
			"DDL. Body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "sudo_required") {
		t.Errorf("the refusal does not carry the sudo_required code the SPA branches "+
			"on to open the password dialog: %s", w.Body.String())
	}
}

func TestApproveRefusesACrossOriginRequest(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	token := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, token)

	r := f.request(t, "POST", "/api/plans/"+planID+"/approve", `{"reason":"agreed"}`, token)
	r.Header.Set("Origin", "https://evil.example.com")

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("a cross-origin approval got %d, want 403 — any page the operator "+
			"has open could then release a schema change with their cookie. Body: %s",
			w.Code, w.Body.String())
	}
}

func TestApproveSucceedsWithRoleSudoAndOrigin(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	token := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, token)

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/approve", `{"reason":"agreed in review"}`, token))

	if w.Code != http.StatusOK {
		t.Fatalf("a legitimate approval got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "approved") {
		t.Errorf("the response does not report the new state: %s", w.Body.String())
	}
	// An approval nobody can attribute afterwards is not an approval.
	if n := f.auditCount(t, "approve_schema_plan"); n != 1 {
		t.Errorf("%d approval audit rows, want 1", n)
	}
}

// Reject deliberately does not take sudo.
//
// The worst a wrongly-rejected plan costs is a re-plan; putting a password
// prompt in front of "no" is how reviewers stop saying it. Asserted because
// somebody symmetrising the two routes would otherwise change it quietly, and
// the reasoning would be nowhere.
func TestRejectDoesNotRequireSudoButDoesRequireTheRole(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)

	viewer := f.signIn(t, "viewer@example.com", "viewer")
	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/reject", `{"reason":"no"}`, viewer))
	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer rejected a change: got %d, want 403. Anyone who can sign in "+
			"could then block every deploy in the deployment. Body: %s", w.Code, w.Body.String())
	}

	admin := f.signIn(t, "admin@example.com", "admin")
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "POST",
		"/api/plans/"+planID+"/reject", `{"reason":"billing still reads that column"}`, admin))
	if w.Code != http.StatusOK {
		t.Fatalf("an admin without sudo could not reject: got %d: %s", w.Code, w.Body.String())
	}
	if n := f.auditCount(t, "reject_schema_plan"); n != 1 {
		t.Errorf("%d rejection audit rows, want 1", n)
	}
}

// A request that skipped requirePolicyRole must be refused by the server.
//
// This is the property that makes the middleware chain fail safe, and it is
// not the one it looks like. Forwarding the user's role rather than the
// policy's is equivalent — past requirePolicyRole's check the two are the same
// string, and a mutation swapping them changes nothing. What matters is the
// case where the middleware did not run at all: no role reaches the handler,
// the empty string is asserted, and the server refuses because no class's
// approver_role is "".
//
// Driven through a mux registered with the same pattern but a shorter chain,
// which is the shape a reordering mistake produces: the handler reachable
// without requirePolicyRole in front of it.
//
// The mux matters, and the first version of this test did without it. Calling
// the handler directly leaves r.PathValue("id") empty — ServeMux is what fills
// it — so the request was refused with "plan_id is required" and the test
// passed without ever reaching the role check it exists to exercise. It
// asserted a refusal and got one, for a reason unrelated to its own claim.
func TestAHandlerReachedWithoutTheRoleMiddlewareIsRefused(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)
	token := f.signIn(t, "admin@example.com", "admin")
	f.elevate(t, token)

	// auth still runs, so ctxUser is present; requirePolicyRole does not, so
	// ctxPlanRoleKey is absent.
	bare := http.NewServeMux()
	bare.HandleFunc("POST /api/plans/{id}/approve", f.srv.auth(f.srv.handleApproveSchemaPlan))

	w := httptest.NewRecorder()
	bare.ServeHTTP(w, f.request(t, "POST", "/api/plans/"+planID+"/approve", `{"reason":"x"}`, token))

	if strings.Contains(w.Body.String(), "plan_id is required") {
		t.Fatalf("the request never reached the role check — the path value did not "+
			"resolve, so this proves nothing: %s", w.Body.String())
	}
	if w.Code == http.StatusOK {
		t.Fatal("an approval went through with no role asserted. The server is meant to " +
			"refuse an empty role, which is what makes a missing or reordered " +
			"requirePolicyRole fail safe rather than fail open.")
	}
	if n := f.auditCount(t, "approve_schema_plan"); n != 0 {
		t.Errorf("%d audit rows for an approval the server refused", n)
	}
}

// Reading the queue is open to any signed-in user, and closed to anonymous
// ones. A change blocking an engineer's deploy is not a secret from that
// engineer; it is not public either.
func TestTheQueueIsReadableBySignedInUsersAndNobodyElse(t *testing.T) {
	f := newConsoleFixture(t)
	planID := f.pendingPlan(t)

	w := httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/plans?state=pending_approval", "", ""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous request to the approval queue got %d, want 401", w.Code)
	}

	viewer := f.signIn(t, "viewer@example.com", "viewer")
	w = httptest.NewRecorder()
	f.srv.handler.ServeHTTP(w, f.request(t, "GET", "/api/plans?state=pending_approval", "", viewer))
	if w.Code != http.StatusOK {
		t.Fatalf("a signed-in viewer could not read the queue: got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), planID) {
		t.Errorf("the waiting plan is not in the queue response: %s", w.Body.String())
	}
}
