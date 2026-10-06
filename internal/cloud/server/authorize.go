package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/analytics"
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

	if r.URL.Query().Get("prompt") == "reauth" {
		s.handleStaleStepUp(w, r)
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
	s.redirectWithAssertion(w, r, grant)
}

// grantFor resolves the grant for one organisation, or answers the request with
// a page.
func (s *Server) grantFor(w http.ResponseWriter, r *http.Request, user *store.User, org string) (issuer.Grant, bool) {
	grant, err := s.resolveGrant(r.Context(), user, org)
	switch {
	case err == nil:
		return grant, true
	case errors.Is(err, store.ErrNotFound):
		page(w, http.StatusForbidden,
			"You are not a member of that organisation.\n\n"+
				"If you expect to be, ask an administrator there to add you.")
	case errors.Is(err, store.ErrNoConsole):
		// An organisation provisions itself, so the answer is what it is
		// waiting on rather than a command to run.
		page(w, http.StatusServiceUnavailable, s.notReadyMessage(r.Context(), org))
	default:
		page(w, http.StatusInternalServerError, "Could not sign you in. Please try again.")
	}
	return issuer.Grant{}, false
}

// resolveGrant reads the membership and destination for one organisation.
//
// It reports store.ErrNotFound when the user is not a member and
// store.ErrNoConsole when the organisation has no console yet.
func (s *Server) resolveGrant(ctx context.Context, user *store.User, org string) (issuer.Grant, error) {
	// Not a member is a permission answer and no console is an operator answer,
	// so the two are reported apart.
	role, err := s.db.RoleIn(ctx, user.ID, org)
	if errors.Is(err, store.ErrNotFound) {
		// The same answer whether the organisation does not exist or the user is
		// not in it. Distinguishing them makes this route an enumeration of
		// every organisation in the product.
		s.log.Info("authorize refused: no membership", "user", user.ID, "org", org)
		return issuer.Grant{}, err
	}
	if err != nil {
		s.log.Error("read membership", "user", user.ID, "org", org, "err", err)
		return issuer.Grant{}, err
	}

	consoleURL, err := s.db.ConsoleURL(ctx, org)
	if err != nil {
		if !errors.Is(err, store.ErrNoConsole) {
			s.log.Error("read console url", "org", org, "err", err)
		}
		return issuer.Grant{}, err
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
	}, nil
}

// redirectWithAssertion mints and sends the browser on.
//
// The assertion travels in the URL fragment. A fragment is never sent to a
// server, so it stays out of the console's access log, out of the Referer on
// the next navigation, and out of anything between the two. The console strips
// it from the address bar before exchanging it.
func (s *Server) redirectWithAssertion(w http.ResponseWriter, r *http.Request, grant issuer.Grant) {
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
	if next := r.URL.Query().Get("return_to"); next != "" {
		frag.Set("return_to", next)
	}

	s.reportAuthorized(grant)
	http.Redirect(w, r, grant.Audience+"/login#"+frag.Encode(), http.StatusSeeOther)
}

// reportAuthorized logs and records an assertion minted for a console.
func (s *Server) reportAuthorized(grant issuer.Grant) {
	s.log.Info("authorized", "user", grant.Subject, "org", grant.Org,
		"role", grant.Role, "step_up", grant.StepUp)
	// The handoff into a console is what entering the product looks like with a
	// Cloud session already held; account.signed_in covers only the sign-in
	// that creates one.
	s.capture(analytics.EventConsoleAuthorized, grant.Subject, "", grant.Org, map[string]any{
		"role":    grant.Role,
		"step_up": grant.StepUp,
	})
}

// originOf reduces a URL to its origin in the form a browser's Origin header
// takes — lowercase, without a default port — or "" when it has none.
//
// Migration 0004's CHECK is `console_url ~ '^https?://[^/]+'`, unanchored at
// the end, so it admits `https://example.com/console`, and `cloud org register`
// permits a path and any case too. A registered console URL is compared to an
// Origin header only after both pass through this.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// requireSessionPage resolves a session for a route a browser navigates to,
// answering with a page when there is none.
func (s *Server) requireSessionPage(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u, ok := s.sessionUser(r)
	if !ok {
		page(w, http.StatusUnauthorized,
			"Sign in to Atlantis Cloud first, then try this again.")
		return nil, false
	}
	return u, true
}
