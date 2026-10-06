package server

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Refusal codes for the step-up route. The console reads them to choose what
// to show; the message beside each is for the person.
const (
	stepUpSessionRequired = "session_required"
	stepUpCodeRejected    = "code_rejected"
	stepUpNotMember       = "not_member"
	stepUpNoConsole       = "no_console"
	stepUpWrongConsole    = "wrong_console"
)

// handleStepUp checks a second factor typed into an organisation's console and
// answers with a step-up assertion for that console. It is the only route that
// sets identity.Claims.StepUp.
//
// The caller must be the organisation's registered console, calling with fetch
// from its own origin, same-site with Cloud so the session cookie travels.
func (s *Server) handleStepUp(w http.ResponseWriter, r *http.Request) {
	// First, so the database reads below are bounded. A 429 carries no CORS
	// headers and reaches the console as a network error.
	if !s.rateLimited(w, r) {
		return
	}
	ctx := r.Context()

	// Any registered console's origin gets CORS headers, so the console can
	// read every refusal below. The organisation's own console is checked
	// after membership, so an unknown organisation and one the user is not in
	// answer the same.
	origin := originOf(r.Header.Get("Origin"))
	if origin == "" || !s.isConsoleOrigin(r, origin) {
		jsonError(w, "this request must come from an organisation's console", http.StatusForbidden)
		return
	}
	allowConsole(w, r.Header.Get("Origin"))

	// A JSON body is what makes a cross-origin caller send a preflight first.
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
		jsonError(w, "send the code as application/json", http.StatusUnsupportedMediaType)
		return
	}

	user, ok := s.sessionUser(r)
	if !ok {
		stepUpRefusal(w, http.StatusUnauthorized, stepUpSessionRequired,
			"Your Atlantis Cloud session has ended. Sign in at Atlantis Cloud, then try again.")
		return
	}

	org := r.PathValue("org")
	grant, err := s.resolveGrant(ctx, user, org)
	switch {
	case errors.Is(err, store.ErrNotFound):
		stepUpRefusal(w, http.StatusForbidden, stepUpNotMember,
			"You are not a member of that organisation.")
		return
	case errors.Is(err, store.ErrNoConsole):
		stepUpRefusal(w, http.StatusServiceUnavailable, stepUpNoConsole, s.notReadyMessage(ctx, org))
		return
	case err != nil:
		jsonError(w, "could not check your code — please try again", http.StatusInternalServerError)
		return
	}

	// The assertion's audience is the organisation's console URL, so another
	// organisation's console could not use it, but it must not receive it.
	if originOf(grant.Audience) != origin {
		stepUpRefusal(w, http.StatusForbidden, stepUpWrongConsole,
			"This console is not the one registered for that organisation.")
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}

	accepted, err := s.checkSecondFactor(ctx, user.ID, strings.TrimSpace(body.Code))
	if err != nil {
		s.log.Error("check second factor", "user", user.ID, "err", err)
		jsonError(w, "could not check your code — please try again", http.StatusInternalServerError)
		return
	}
	if !accepted {
		// SpendTOTPStep accepts a code only from a step later than the last one
		// used, so the code still on screen is refused however correct it
		// looks. The rate limiter bounds guessing.
		stepUpRefusal(w, http.StatusUnauthorized, stepUpCodeRejected,
			"That code is not right, or it has already been used. "+
				"Enter the next code from your authenticator, or an unused backup code.")
		return
	}

	grant.StepUp = true
	token, err := s.iss.Mint(grant)
	if err != nil {
		s.log.Error("mint assertion", "user", grant.Subject, "org", grant.Org, "err", err)
		jsonError(w, "could not confirm it is you — please try again", http.StatusInternalServerError)
		return
	}
	s.reportAuthorized(grant)
	writeJSON(w, http.StatusOK, map[string]string{"assertion": token})
}

// handleStepUpPreflight answers the CORS preflight a JSON POST from a console
// triggers, for any Origin.
func (s *Server) handleStepUpPreflight(w http.ResponseWriter, r *http.Request) {
	// A preflight carries no cookie and reads nothing, and handleStepUp refuses
	// an origin that is not a registered console before it reads the session.
	// Answering every origin keeps database reads off a route the rate limiter
	// does not cover.
	if origin := r.Header.Get("Origin"); origin != "" {
		allowConsole(w, origin)
		h := w.Header()
		h.Set("Access-Control-Allow-Methods", "POST")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStaleStepUp answers a console page that still asks for a second factor
// through Cloud's popup, at GET /authorize?prompt=reauth or POST
// /authorize/reauth.
func (s *Server) handleStaleStepUp(w http.ResponseWriter, _ *http.Request) {
	// An ordinary assertion minted here would be spent by the popup on a
	// second console session, and the dialog that opened it would wait.
	page(w, http.StatusBadRequest, "This console page is out of date.\n\n"+
		"Close this window and reload the console. It now asks for your code itself.")
}

// isConsoleOrigin reports whether origin, already reduced by originOf, is the
// origin of some registered console. A database error answers false.
func (s *Server) isConsoleOrigin(r *http.Request, origin string) bool {
	urls, err := s.db.ConsoleURLs(r.Context())
	if err != nil {
		s.log.Error("read console urls", "err", err)
		return false
	}
	for _, u := range urls {
		if originOf(u) == origin {
			return true
		}
	}
	return false
}

// allowConsole lets a page on origin read the answer and send the cookie.
func allowConsole(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
}

// stepUpRefusal writes a refusal the console can act on: code is one of the
// stepUp* constants above.
func stepUpRefusal(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
