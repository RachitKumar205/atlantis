package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The signed-in half of Cloud's API.
//
// Everything else Cloud serves answers for somebody who is not signed in yet —
// /api/auth/config is deliberately unauthenticated, /api/auth/pending reads a
// pre-session cookie — or hands a browser off to a console. These are the first
// routes that answer "who are you, and what do you have".
//
// That is also why the sign-in application has never had a signed-in state:
// there was nothing to ask. It boots on config and pending, both pre-session,
// and finishing a sign-in redirects to /organisations, which reloads into the
// same state machine.

// meResponse is the shape the SPA boots on.
//
// snake_case on the wire, matching every other Cloud route; web/cloud converts
// at its own boundary rather than carrying the wire shape inward.
type meResponse struct {
	Email string        `json:"email"`
	Name  string        `json:"name,omitempty"`
	Orgs  []orgResponse `json:"orgs"`

	// OrgLimit and OrgsCreated are what the screen needs to decide whether to
	// offer a create form at all, rather than offering one that always refuses.
	OrgLimit    int `json:"org_limit"`
	OrgsCreated int `json:"orgs_created"`
}

// orgResponse is one organisation as its member sees it.
//
// # What is deliberately absent
//
// The last provisioning error. It is written for an operator and carries image
// references, cluster hostnames and API paths — one produced by the local
// walkthrough contained the whole Kubernetes API server URL. A customer whose
// organisation failed gets the state, the attempt count and somewhere to ask;
// the detail stays in `cloud org status`.
type orgResponse struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`

	// State is empty for the organisations registered by hand, which have no
	// queue row and never will.
	State    string `json:"state"`
	Attempts int    `json:"attempts"`

	// URL is where to send somebody who clicks this organisation, and it is
	// built here rather than assembled by the client from a name it holds.
	// Always /authorize, never a console directly: the membership re-read is
	// the gate, and a client that linked straight to a console would skip it.
	//
	// Empty until the organisation is provisioned, which is what the screen
	// renders as "still coming up" rather than a dead link.
	URL string `json:"url,omitempty"`

	// CreatedByMe gates what this account may do to it later. Deletion is P4b;
	// this is here now because the screen has to know whose organisation it is
	// to say so.
	CreatedByMe bool `json:"created_by_me"`
}

// handleMe reports the signed-in account and its organisations.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	orgs, err := s.db.OrgsForUser(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list organisations", "user", user.ID, "err", err)
		jsonError(w, "could not read your organisations", http.StatusInternalServerError)
		return
	}

	limit, err := s.db.OrgLimitFor(r.Context(), user.ID)
	if err != nil {
		s.log.Error("read organisation limit", "user", user.ID, "err", err)
		jsonError(w, "could not read your organisations", http.StatusInternalServerError)
		return
	}

	out := meResponse{
		Email:    user.Email,
		Name:     user.Name,
		Orgs:     make([]orgResponse, 0, len(orgs)),
		OrgLimit: limit,
	}
	for _, o := range orgs {
		if o.CreatedByMe {
			out.OrgsCreated++
		}
		out.Orgs = append(out.Orgs, s.orgResponse(o))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetOrg reports one organisation, for a screen waiting on it.
//
// Membership-gated, and an organisation the caller is not in is reported as
// absent — the same answer as one that does not exist. Distinguishing them
// would let anybody enumerate which organisations are registered.
func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	org := r.PathValue("org")

	o, err := s.db.OrgForUser(r.Context(), user.ID, org)
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, "no such organisation", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("read organisation", "org", org, "err", err)
		jsonError(w, "could not read that organisation", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s.orgResponse(*o))
}

// orgResponse converts one summary for the wire.
func (s *Server) orgResponse(o store.OrgSummary) orgResponse {
	out := orgResponse{
		Name:        o.Name,
		DisplayName: o.DisplayName,
		Role:        string(o.Role),
		State:       string(o.State),
		Attempts:    o.Attempts,
		CreatedByMe: o.CreatedByMe,
	}
	// Only once there is a console to reach. /authorize would answer its own
	// 503 otherwise, and a link that reliably fails is worse than no link.
	if o.ConsoleURL != "" {
		out.URL = s.cfg.PublicURL + "/authorize?org=" + url.QueryEscape(o.Name)
	}
	return out
}

// sameOrigin refuses a cross-site state-changing request, answering it if so.
//
// # Why this is required here and not on the form routes
//
// Cloud's session cookie is SameSiteLaxMode, not the console's Strict, and for
// a stated reason: verification and reset links arrive as top-level navigations
// from a mail client, and Strict drops the cookie on exactly those. Lax already
// blocks a cross-site POST, so this is a second answer to the same question
// rather than the only one.
//
// The console's version of this check allows a request with no Origin header
// and leans on Strict to cover that. Cloud cannot lean on Strict, so this
// requires the header — but only where requiring it is safe.
//
// It is safe on /api/* and nowhere else. Those are reached by fetch in cors
// mode, which always appends a real Origin. The form posts — POST /reset and
// POST /authorize/reauth — are reached as HTML form navigations, and
// securityHeaders sets Referrer-Policy: no-referrer on every response, under
// which a form navigation sends `Origin: null`. Requiring the header there
// would refuse the two flows the Lax cookie exists to protect, which is the
// mistake an earlier draft of this made.
func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		jsonError(w, "this request must carry an Origin header", http.StatusForbidden)
		return false
	}
	if !s.originMatches(origin, r) {
		jsonError(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	return true
}

// originMatches compares an Origin against where this server believes it is.
//
// Compared against the configured public URL rather than r.Host, because the
// two disagree in the one setup a developer uses every day: Vite's proxy sets
// changeOrigin, rewriting Host to Cloud's address while the browser still sends
// the page's own origin. A host comparison would refuse every write under
// `vite dev` and be discovered by whoever next ran the frontend.
//
// r.Host is still accepted, for a deployment reached at a name the public URL
// does not name — a private ingress, a health probe from inside the cluster.
func (s *Server) originMatches(origin string, r *http.Request) bool {
	if strings.EqualFold(origin, s.cfg.PublicURL) {
		return true
	}
	for _, extra := range s.cfg.ExtraOrigins {
		if strings.EqualFold(origin, extra) {
			return true
		}
	}
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return strings.EqualFold(host, r.Host)
}

// handleCreateOrg creates an organisation and queues it for provisioning.
//
// The route this whole effort exists to make possible: before it, an
// organisation came from `cloud org create` at a terminal, and /authorize told
// anybody who got ahead of that to go and find an operator.
func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	if !s.sameOrigin(w, r) {
		return
	}

	var body struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if !decode(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)

	// Validated here as well as in the store, so the message is about the name
	// rather than about a database. The store validates too, because it is
	// reachable from `cloud org create` and must not depend on a caller having
	// been careful.
	if err := identity.ValidateOrgName(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := s.db.CreateOrgForOwner(r.Context(), name, strings.TrimSpace(body.DisplayName),
		user.ID, identity.RoleAdmin)
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		// "Not available" rather than "taken", and the same answer a reserved
		// name gets. Not to hide that the name exists — names are globally
		// unique, so anybody can discover that by trying, and pretending
		// otherwise would be theatre. It is because to the person typing, taken
		// and reserved are the same fact and have the same remedy.
		jsonError(w, "that name is not available", http.StatusConflict)
		return
	case errors.Is(err, store.ErrOrgLimitReached):
		jsonError(w, "you have reached the number of organisations this account may create",
			http.StatusForbidden)
		return
	case err != nil:
		s.log.Error("create organisation", "org", name, "user", user.ID, "err", err)
		jsonError(w, "could not create that organisation", http.StatusInternalServerError)
		return
	}

	// The first human-actor row in cloud.audit_log — until now the provisioner
	// was its only writer. The actor's email is written onto the row rather than
	// resolved later, so it says who acted then and not who holds that identity
	// now.
	s.db.LogAction(r.Context(), name, user.ID, user.Email, "org.created", map[string]any{
		"display_name": body.DisplayName,
	})

	o, err := s.db.OrgForUser(r.Context(), user.ID, name)
	if err != nil {
		// Created, but unreadable a moment later. Report the creation rather
		// than an error: the organisation exists and is queued, and a client
		// that retried would be told the name is taken — by itself.
		s.log.Error("read back a created organisation", "org", name, "err", err)
		writeJSON(w, http.StatusCreated, orgResponse{Name: name, Role: string(identity.RoleAdmin),
			State: string(store.StatePending), CreatedByMe: true})
		return
	}
	writeJSON(w, http.StatusCreated, s.orgResponse(*o))
}

// notReadyMessage explains why an organisation cannot be entered yet.
//
// Reads the queue rather than saying one thing for every case, because the four
// states want four different answers and only one of them is "wait".
//
// Deliberately says nothing about *why* a failure failed. The reason is in
// last_error, which is written for an operator — see orgResponse. Somebody
// locked out of their organisation is the last person who should be handed a
// Kubernetes API path.
func (s *Server) notReadyMessage(ctx context.Context, org string) string {
	p, err := s.db.ProvisioningFor(ctx, org)
	if err != nil {
		// No queue row at all: registered by hand, or created before any of
		// this existed. Nothing is coming to finish it.
		return "That organisation is not ready yet.\n\n" +
			"If this does not resolve, ask whoever set it up."
	}
	switch p.State {
	case store.StatePending, store.StateProvisioning:
		return "That organisation is still being set up.\n\n" +
			"This usually takes about a minute. Try again shortly."
	case store.StateFailed:
		return "That organisation could not be set up.\n\n" +
			"It will be retried automatically. If it stays this way, contact support."
	default:
		// ready, but no console URL — the two writes registration makes, caught
		// between them. It resolves on its own.
		return "That organisation is nearly ready. Try again shortly."
	}
}
