package server

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// handleAuthorize sends a signed-in user to an organisation's console.
//
// The role in the assertion is read from cloud.memberships. Unlike `cloud
// mint`, which signs the -org and -role it is given and requires the signing
// key, this route is reachable by any session, so it takes no role parameter.
//
// The destination is looked up from the organisation. The request carries no
// URL, so there is no allowlist to write wrongly. A `console` query parameter
// is ignored, and a test asserts that.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	user, ok := s.requireSessionPage(w, r)
	if !ok {
		return
	}

	org := r.URL.Query().Get("org")
	if org == "" {
		page(w, http.StatusBadRequest, "Which organisation? Add ?org= to this address.")
		return
	}

	grant, ok := s.grantFor(w, r, user, org)
	if !ok {
		return
	}

	// prompt=reauth means the caller wants an assertion that says a factor was
	// presented. A session is not that, however recent — see the reauth page.
	if r.URL.Query().Get("prompt") == "reauth" {
		s.serveReauthPage(w, org, grant.Audience, "")
		return
	}

	s.redirectWithAssertion(w, r, grant, false)
}

// grantFor resolves the membership and destination for one organisation, or
// answers the request.
//
// Both lookups together, because they fail for different reasons: not a member
// is a permission answer, and no console registered is an operator answer.
func (s *Server) grantFor(w http.ResponseWriter, r *http.Request, user *store.User, org string) (issuer.Grant, bool) {
	ctx := r.Context()

	role, err := s.db.RoleIn(ctx, user.ID, org)
	if errors.Is(err, store.ErrNotFound) {
		// The same answer whether the organisation does not exist or the user is
		// not in it. Distinguishing them makes this route an enumeration of
		// every organisation in the product.
		s.log.Info("authorize refused: no membership", "user", user.ID, "org", org)
		page(w, http.StatusForbidden,
			"You are not a member of that organisation.\n\n"+
				"If you expect to be, ask an administrator there to add you.")
		return issuer.Grant{}, false
	}
	if err != nil {
		s.log.Error("read membership", "user", user.ID, "org", org, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign you in. Please try again.")
		return issuer.Grant{}, false
	}

	consoleURL, err := s.db.ConsoleURL(ctx, org)
	if errors.Is(err, store.ErrNoConsole) {
		// An organisation provisions itself, so the answer is what it is
		// waiting on rather than a command to run.
		page(w, http.StatusServiceUnavailable, s.notReadyMessage(ctx, org))
		return issuer.Grant{}, false
	}
	if err != nil {
		s.log.Error("read console url", "org", org, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign you in. Please try again.")
		return issuer.Grant{}, false
	}

	// Every organisation this person belongs to, so the console can draw a
	// switcher without calling Cloud on each page.
	//
	// A failure here is not fatal. The gate is this function, and the list only
	// draws the switcher, so minting without it costs a menu.
	var orgs []string
	memberships, err := s.db.MembershipsOf(ctx, user.ID)
	if err != nil {
		s.log.Warn("read memberships for the org list", "user", user.ID, "err", err)
	}
	// Display names only where they differ from the name. An organisation that
	// calls itself what it is called adds nothing to the token, and a console
	// that finds no entry draws the name it already has.
	var orgNames map[string]string
	for _, m := range memberships {
		orgs = append(orgs, m.Org)
		if m.DisplayName != "" && m.DisplayName != m.Org {
			if orgNames == nil {
				orgNames = make(map[string]string, len(memberships))
			}
			orgNames[m.Org] = m.DisplayName
		}
	}

	return issuer.Grant{
		Subject: user.ID,
		Org:     org,
		Role:    role,
		Email:   user.Email,
		Name:    user.Name,
		// The destination is also the audience. One value, so a token cannot be
		// delivered somewhere it would not verify.
		Audience: consoleURL,
		// Names only, no roles: the list draws a menu, and a console has no
		// use for permissions in an organisation it cannot reach.
		Orgs:     orgs,
		OrgNames: orgNames,
	}, true
}

// redirectWithAssertion mints and sends the browser on.
//
// The assertion travels in the URL fragment. A fragment is never sent to a
// server, so it stays out of the console's access log, out of the Referer on
// the next navigation, and out of anything between the two. The console strips
// it from the address bar before exchanging it.
func (s *Server) redirectWithAssertion(w http.ResponseWriter, r *http.Request, grant issuer.Grant, stepUp bool) {
	grant.StepUp = stepUp

	token, err := s.iss.Mint(grant)
	if err != nil {
		s.log.Error("mint assertion", "user", grant.Subject, "org", grant.Org, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign you in. Please try again.")
		return
	}

	frag := url.Values{"assertion": {token}}

	// The path the console was trying to reach when it sent the browser here.
	//
	// Opaque to Cloud, which never navigates to it. The console reduces it to a
	// path on its own origin before using it, so a value naming another host
	// moves nothing.
	//
	// Omitted for step-up: that assertion goes to a popup which closes, and the
	// page underneath has not moved.
	if next := r.URL.Query().Get("return_to"); next != "" && !stepUp {
		frag.Set("return_to", next)
	}

	if stepUp {
		// Tells the console's sign-in page that it is running in a popup opened
		// for step-up, so it hands the assertion to its opener rather than
		// exchanging it for a second session.
		frag.Set("mode", "reauth")
	}

	s.log.Info("authorized", "user", grant.Subject, "org", grant.Org,
		"role", grant.Role, "step_up", stepUp)
	http.Redirect(w, r, grant.Audience+"/login#"+frag.Encode(), http.StatusSeeOther)
}

// originOf reduces a URL to a CSP source expression.
//
// A source expression may carry a path, but matching then becomes path-prefix
// matching and the redirect target here is /login — so anything but the bare
// origin risks a policy that looks right and blocks the one navigation this
// page exists to make.
//
// Migration 0004's CHECK is `console_url ~ '^https?://[^/]+'`, unanchored at
// the end, so it admits `https://example.com/console`, and `cloud org register`
// permits a path too. Reducing to the origin keeps the directive correct.
//
// An unparseable value yields the empty string, which leaves form-action at
// 'self' — the page still renders and the redirect is still refused, which is
// the same failure as before rather than a new one. It cannot be reached from
// a registered organisation.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// serveReauthPage asks for a second factor.
//
// The console opens this in a popup, so unlike the JSON sign-in routes it is
// navigated to by a browser.
//
// Script-free, so Cloud stays under `default-src 'none'`. The postMessage that
// hands the result back runs on the console's origin, under the console's own
// script-src, after the redirect below.
func (s *Server) serveReauthPage(w http.ResponseWriter, org, consoleURL, errMsg string) {
	banner := ""
	if errMsg != "" {
		banner = `<p><strong>` + html.EscapeString(errMsg) + `</strong></p>`
	}
	w.Header().Set("Cache-Control", "no-store")

	// form-action has to name the console, and `'self'` alone silently breaks
	// this page.
	//
	// The form POSTs same-origin to /authorize/reauth, which is what `'self'`
	// covers — but that handler answers 303 to the console's origin, and
	// browsers enforce form-action ACROSS the redirect chain. With `'self'`
	// only, Chrome and Firefox accept the POST, let the server mint the
	// assertion, and then refuse to follow the redirect.
	//
	// The failure has no symptom. The page does not navigate and no error is
	// shown, so the operator presses the button again, resends a code that is
	// now spent, and is told the code is wrong — which is the one explanation
	// that is not true. It cost an afternoon to find.
	//
	// The console's origin is not a wildcard: it is cloud.orgs.console_url for
	// this organisation, the same value used as the assertion's audience, so an
	// assertion cannot be posted anywhere it would not verify. Only the origin
	// is used — the column is constrained to scheme://host[:port] — because a
	// CSP source expression with a path would not match.
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"form-action 'self' " + originOf(consoleURL),
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">` +
		`<title>Confirm it is you</title>` +
		`<h1>Confirm it is you</h1>` +
		`<p>This action needs your second factor, even though you are signed in.</p>` +
		banner +
		`<form method="post" action="/authorize/reauth">` +
		// The organisation travels in a hidden field rather than the query
		// string, because the POST target is fixed and this is the only thing
		// the form needs to carry. It is escaped: it is attacker-supplied and
		// is going into an attribute.
		`<input type="hidden" name="org" value="` + html.EscapeString(org) + `">` +
		`<label>Code from your authenticator ` +
		`<input name="code" inputmode="numeric" autocomplete="one-time-code" ` +
		`autofocus required></label> ` +
		`<button type="submit">Confirm</button>` +
		`</form>` +
		`<p>A backup code works here too.</p>`))
}

// handleReauth checks the second factor and mints a step-up assertion.
//
// A step-up assertion asserts that a factor was presented; an ordinary one
// asserts only a session, which may be twelve hours old. This is the only route
// that sets identity.Claims.StepUp, and only after checkSecondFactor returns
// true.
func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	user, ok := s.requireSessionPage(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		page(w, http.StatusBadRequest, "That form could not be read. Please try again.")
		return
	}
	org := r.PostFormValue("org")

	// Membership is re-checked here rather than trusted from the form. The form
	// was served to this user, but a POST is not obliged to have come from it,
	// and an org read out of a request body is a request parameter like any
	// other.
	grant, ok := s.grantFor(w, r, user, org)
	if !ok {
		return
	}

	accepted, err := s.checkSecondFactor(r.Context(), user.ID, r.PostFormValue("code"))
	if err != nil {
		s.log.Error("check second factor", "user", user.ID, "err", err)
		page(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	if !accepted {
		// The page comes back rather than a dead end, and the session survives:
		// a mistyped digit should cost a retry, not the sign-in. The rate
		// limiter is what bounds guessing.
		// "Try the next one" and not "try again": SpendTOTPStep accepts a code
		// only from a step strictly later than the last one used, so
		// resubmitting the code still on screen is refused however correct it
		// looks, which is what stops a code observed over a shoulder being
		// replayed inside its ninety-second validity.
		s.serveReauthPage(w, org, grant.Audience,
			"That code is not right, or it has already been used. "+
				"Wait for your authenticator to show a new one.")
		return
	}

	s.redirectWithAssertion(w, r, grant, true)
}

// requireSessionPage resolves a session for a route a browser navigates to.
//
// requireSession answers JSON, which is right for the account routes and wrong
// here — these two are reached by a person following a link or a popup, and a
// JSON error body is not something they can act on.
func (s *Server) requireSessionPage(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u, ok := s.sessionUser(r)
	if !ok {
		page(w, http.StatusUnauthorized,
			"Sign in to Atlantis Cloud first, then try this again.")
		return nil, false
	}
	return u, true
}
