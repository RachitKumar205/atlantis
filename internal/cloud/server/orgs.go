package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The signed-in half of Cloud's API.
//
// Every other route answers before a session exists — /api/auth/config is
// unauthenticated and /api/auth/pending reads a pre-session cookie — or hands
// the browser off to a console. These are the first routes that read a session.
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
	// UserID is what the browser reports analytics under, so a page's events
	// and this server's land on one person rather than two. The same value
	// every session already carries.
	UserID string `json:"user_id"`

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
// The last provisioning error is not included. It carries image references,
// cluster hostnames and API paths; one produced locally contained the whole
// Kubernetes API server URL. It is reachable through `cloud org status`.
type orgResponse struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`

	// State is empty for the organisations registered by hand, which have no
	// queue row and never will.
	State    string `json:"state"`
	Attempts int    `json:"attempts"`

	// URL is the destination for this organisation, built here rather than
	// assembled by the client from a name it holds.
	// Always /authorize, never a console directly: the membership re-read is
	// the gate, and a client that linked straight to a console would skip it.
	//
	// Empty until the organisation is provisioned, which is what the screen
	// renders as "still coming up" rather than a dead link.
	URL string `json:"url,omitempty"`

	// CreatedByMe is whose organisation it is, which the screen says out loud.
	//
	// It does not gate deletion. cloud.orgs.created_by is nullable, so an
	// organisation whose creator closed their account would be undeletable;
	// the guard is an admin membership. See store.SoftDeleteOrg.
	CreatedByMe bool `json:"created_by_me"`

	// PurgeAfter is when a deleted organisation stops being restorable, RFC3339,
	// and empty in every other state.
	//
	// Sent rather than computed by the client: the window is a property of the
	// row. The screen renders the date.
	PurgeAfter string `json:"purge_after,omitempty"`
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
		UserID:   user.ID,
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
// Membership-gated. An organisation the caller is not in reports as absent,
// the same answer one that does not exist gets, so the route cannot enumerate
// registered organisations.
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
	if o.PurgeAfter != nil {
		out.PurgeAfter = o.PurgeAfter.UTC().Format(time.RFC3339)
	}
	// Only once there is a console to reach; /authorize would answer its own
	// 503 otherwise.
	if o.ConsoleURL != "" {
		out.URL = s.cfg.PublicURL + "/authorize?org=" + url.QueryEscape(o.Name)
	}
	return out
}

// sameOrigin refuses a cross-site state-changing request, answering it if so.
//
// Applies to /api/* only. Cloud's session cookie is SameSiteLaxMode rather than
// the console's Strict, because verification and reset links arrive as
// top-level navigations from a mail client and Strict drops the cookie on
// those. Lax already blocks a cross-site POST; this is a second check.
//
// Unlike the console's equivalent, a missing Origin header is refused. That is
// safe on /api/*, which is reached by fetch in cors mode and always carries a
// real Origin. It is not safe on the form posts: securityHeaders sets
// Referrer-Policy: no-referrer, under which POST /reset and
// POST /authorize/reauth send `Origin: null`.
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
// Compared against the configured public URL, which Vite's proxy makes differ
// from r.Host: changeOrigin rewrites Host to Cloud's address while the browser
// still sends the page's own origin.
//
// r.Host is accepted too, for a deployment reached at a name the public URL
// does not carry, such as a private ingress.
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
		// The same answer a reserved name gets. Both have the same remedy, and
		// names are globally unique, so existence is discoverable by trying.
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

	// The actor's email is written onto the row rather than resolved later, so
	// it records who acted then and not who holds that identity now.
	s.db.LogAction(r.Context(), name, user.ID, user.Email, "org.created", map[string]any{
		"display_name": body.DisplayName,
	})

	o, err := s.db.OrgForUser(r.Context(), user.ID, name)
	if err != nil {
		// Created but unreadable. Reported as created, since it exists and is
		// queued, and a retry answers that the name is taken.
		s.log.Error("read back a created organisation", "org", name, "err", err)
		writeJSON(w, http.StatusCreated, orgResponse{Name: name, Role: string(identity.RoleAdmin),
			State: string(store.StatePending), CreatedByMe: true})
		return
	}
	writeJSON(w, http.StatusCreated, s.orgResponse(*o))
}

// notReadyMessage explains why an organisation cannot be entered yet.
//
// Reads the queue, because the four states have four different answers and
// only one of them is "wait".
//
// It says nothing about why a failure failed. last_error is written for an
// operator and carries image references and cluster hostnames; see orgResponse.
func (s *Server) notReadyMessage(ctx context.Context, org string) string {
	p, err := s.db.ProvisioningFor(ctx, org)
	if err != nil {
		// No queue row: registered by hand. Nothing will finish it.
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

// deleteWindow is how long a deleted organisation stays restorable.
//
// A constant rather than configuration: the window is quoted at the moment of
// deletion, and a per-deployment value gives two customers different answers
// from the same product.
//
// It is passed to SoftDeleteOrg rather than read there, and the resulting date
// is stored on the row. So changing this number affects organisations deleted
// afterwards and never the ones already counting down.
const deleteWindow = 30 * 24 * time.Hour

// handleDeleteOrg soft-deletes an organisation.
//
// It destroys nothing: it writes a row, and the provisioner tears the namespace
// down once deleteWindow elapses. No Cloud route reaches the cluster.
//
// The body must carry the organisation's own name, compared server-side, since
// the request can be issued without the client's confirmation dialog.
func (s *Server) handleDeleteOrg(w http.ResponseWriter, r *http.Request) {
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
	org := r.PathValue("org")

	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Confirm) != org {
		jsonError(w, "type the organisation's name to confirm", http.StatusBadRequest)
		return
	}

	err := s.db.SoftDeleteOrg(r.Context(), org, user.ID, deleteWindow)
	switch {
	case errors.Is(err, store.ErrNotPermitted):
		// Distinct from the not-found below. This account is a member and
		// already knows the organisation exists, so naming the reason
		// discloses nothing.
		jsonError(w, "only an admin of this organisation may delete it", http.StatusForbidden)
		return
	case errors.Is(err, store.ErrNotFound):
		// Covers both "no such organisation, or you are not a member" and "it
		// is not in a state that can be deleted". Not distinguished, because
		// the first must not confirm which organisations exist and the second
		// is answered by re-reading the state.
		jsonError(w, "that organisation cannot be deleted", http.StatusNotFound)
		return
	case err != nil:
		s.log.Error("delete organisation", "org", org, "user", user.ID, "err", err)
		jsonError(w, "could not delete that organisation", http.StatusInternalServerError)
		return
	}

	s.db.LogAction(r.Context(), org, user.ID, user.Email, "org.deleted", map[string]any{
		"restorable_for": deleteWindow.String(),
	})

	o, err := s.db.OrgForUser(r.Context(), user.ID, org)
	if err != nil {
		s.log.Error("read back a deleted organisation", "org", org, "err", err)
		writeJSON(w, http.StatusOK, orgResponse{Name: org, State: string(store.StateDeleted)})
		return
	}
	writeJSON(w, http.StatusOK, s.orgResponse(*o))
}

// handleRestoreOrg returns a soft-deleted organisation to service.
//
// A real restore rather than a rebuild: the namespace was never touched, so the
// same certificate authority and the same database come back. A rebuild would
// mint a new authority and every enrolled caller would have to run `tide login`
// again — which is the difference between undoing a deletion and replacing an
// organisation with one that shares its name.
//
// No confirmation. The guard on delete exists because the action is
// destructive; this one puts something back.
func (s *Server) handleRestoreOrg(w http.ResponseWriter, r *http.Request) {
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
	org := r.PathValue("org")

	err := s.db.RestoreOrg(r.Context(), org, user.ID)
	switch {
	case errors.Is(err, store.ErrNotPermitted):
		jsonError(w, "only an admin of this organisation may restore it", http.StatusForbidden)
		return
	case errors.Is(err, store.ErrNotFound):
		// Also the answer for an organisation the reaper already destroyed.
		// There is nothing to say beyond this: the row is gone.
		jsonError(w, "that organisation cannot be restored", http.StatusNotFound)
		return
	case err != nil:
		s.log.Error("restore organisation", "org", org, "user", user.ID, "err", err)
		jsonError(w, "could not restore that organisation", http.StatusInternalServerError)
		return
	}

	s.db.LogAction(r.Context(), org, user.ID, user.Email, "org.restored", nil)

	o, err := s.db.OrgForUser(r.Context(), user.ID, org)
	if err != nil {
		s.log.Error("read back a restored organisation", "org", org, "err", err)
		writeJSON(w, http.StatusOK, orgResponse{Name: org, State: string(store.StateReady)})
		return
	}
	writeJSON(w, http.StatusOK, s.orgResponse(*o))
}
