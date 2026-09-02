package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The CLI grant flow: how `tide login` gets an assertion with no browser of
// its own. RFC 8628's shape — the CLI opens a grant and polls it; the person
// signs in on any browser, types the short code, and approves.
//
// The person types the code rather than following a link that carries it.
// A link with the code embedded is a phishing primitive: mail it to a victim
// and their click approves the attacker's machine. Typing keeps the decision
// on the screen that showed the code.
//
// Approval runs on the session alone. The session was established with a
// second factor; no fresh factor is collected here.

// cliEnrollPurpose is the purpose claim on every assertion this flow mints.
// The enrolment listener requires it; the session exchange refuses it.
const cliEnrollPurpose = "cli-enroll"

// cliPollInterval is what the CLI is told to wait between polls, seconds.
const cliPollInterval = 5

// cliPollLimit bounds polls per device code per minute: the advertised
// interval with room for clock slop, far under a guessing rate that would
// matter against a 256-bit code.
const cliPollLimit = 20

type cliStartRequest struct {
	Caller   string `json:"caller"`
	Hostname string `json:"hostname"`
}

type cliPollRequest struct {
	DeviceCode string `json:"device_code"`
}

type cliDecideRequest struct {
	UserCode string `json:"user_code"`
	Org      string `json:"org"`
	Approve  bool   `json:"approve"`
}

type cliLookupRequest struct {
	UserCode string `json:"user_code"`
}

// handleCLIStart opens a grant. Unauthenticated: the response is two random
// strings that authorize nothing until a signed-in member approves.
func (s *Server) handleCLIStart(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	var req cliStartRequest
	if !decode(w, r, &req) {
		return
	}
	if !validCallerName(req.Caller) {
		jsonError(w, "caller must be lowercase letters, digits and interior hyphens", http.StatusBadRequest)
		return
	}

	// What the approval page shows. All of it chosen by the requester, none
	// of it verified; it exists so the person sees what is asking, and the
	// console re-checks the caller against its own registry at enrolment.
	meta := map[string]string{
		"hostname": strings.TrimSpace(req.Hostname),
		"address":  s.clientIP(r),
		"agent":    r.UserAgent(),
	}

	deviceCode, userCode, err := s.db.CreateCLIGrant(r.Context(), req.Caller, meta)
	if err != nil {
		s.log.Error("create cli grant", "err", err)
		jsonError(w, "could not start a login", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": deviceCode,
		"user_code":   userCode,
		"verify_url":  s.cfg.PublicURL + "/cli",
		"interval":    cliPollInterval,
		"expires_in":  int(store.CLIGrantTTL.Seconds()),
	})
}

// handleCLIPoll is the CLI's wait. The approved→consumed transition is the
// spend, and the assertion is minted fresh on it — nothing token-shaped rests
// in the table, and a poll arriving late cannot pick up a dead token.
func (s *Server) handleCLIPoll(w http.ResponseWriter, r *http.Request) {
	var req cliPollRequest
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceCode == "" {
		jsonError(w, "device_code is required", http.StatusBadRequest)
		return
	}

	// Limited per code, not per address. A shared NAT polling for two logins
	// must not lock either out — or starve the 5/min sign-in budget the other
	// auth routes share.
	sum := sha256.Sum256([]byte(req.DeviceCode))
	if ok, retry := s.cliPollLim.allow(hex.EncodeToString(sum[:])); !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "slow_down", "retry_in": retry})
		return
	}

	d, err := s.db.ConsumeCLIGrant(r.Context(), req.DeviceCode)
	if errors.Is(err, store.ErrCLIGrantUnusable) {
		jsonError(w, "that login is not usable; run `tide login` again", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Error("consume cli grant", "err", err)
		jsonError(w, "could not check the login", http.StatusInternalServerError)
		return
	}
	if d.Status != "approved" {
		writeJSON(w, http.StatusOK, map[string]any{"status": d.Status})
		return
	}

	// Authorization is read now, not at approval: a membership revoked in the
	// window between the two refuses here.
	user, err := s.db.UserByID(r.Context(), d.UserID)
	if err != nil {
		s.log.Error("read approver", "user", d.UserID, "err", err)
		jsonError(w, "could not complete the login", http.StatusInternalServerError)
		return
	}
	role, err := s.db.RoleIn(r.Context(), d.UserID, d.Org)
	if errors.Is(err, store.ErrNotFound) {
		jsonError(w, "the approving account is no longer a member of that organisation", http.StatusForbidden)
		return
	}
	if err != nil {
		s.log.Error("read role", "user", d.UserID, "org", d.Org, "err", err)
		jsonError(w, "could not complete the login", http.StatusInternalServerError)
		return
	}
	enrollURL, err := s.db.EnrollURL(r.Context(), d.Org)
	if errors.Is(err, store.ErrNoEnrollURL) || errors.Is(err, store.ErrNotFound) {
		jsonError(w, "that organisation has no enrolment address registered", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.log.Error("read enroll url", "org", d.Org, "err", err)
		jsonError(w, "could not complete the login", http.StatusInternalServerError)
		return
	}

	token, err := s.iss.Mint(issuer.Grant{
		Subject: user.ID,
		Org:     d.Org,
		Role:    role,
		Email:   user.Email,
		Name:    user.Name,
		// The audience is where the token is redeemed, as a session
		// assertion's audience is where the browser is sent.
		Audience: enrollURL,
		Purpose:  cliEnrollPurpose,
		Caller:   d.Caller,
	})
	if err != nil {
		s.log.Error("mint cli assertion", "org", d.Org, "err", err)
		jsonError(w, "could not complete the login", http.StatusInternalServerError)
		return
	}

	s.db.LogAction(r.Context(), d.Org, user.ID, user.Email, "cli_grant_consumed",
		map[string]any{"caller": d.Caller})
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "approved",
		"assertion":  token,
		"org":        d.Org,
		"role":       string(role),
		"enroll_url": enrollURL,
	})
}

// handleCLILookup shows a signed-in person what a code is asking for, before
// they decide. A miss charges the grant an attempt — the lookup is the only
// probe surface a user_code has.
func (s *Server) handleCLILookup(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	user, ok := s.sessionUser(r)
	if !ok {
		jsonError(w, "sign in first", http.StatusUnauthorized)
		return
	}
	_ = user
	if !s.rateLimited(w, r) {
		return
	}
	var req cliLookupRequest
	if !decode(w, r, &req) {
		return
	}

	g, err := s.db.CLIGrantByUserCode(r.Context(), req.UserCode)
	if errors.Is(err, store.ErrCLIGrantUnusable) {
		s.db.ChargeCLIGrantAttempt(r.Context(), req.UserCode)
		jsonError(w, "that code is not waiting for approval", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("look up cli grant", "err", err)
		jsonError(w, "could not look that up", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"caller":     g.Caller,
		"hostname":   g.ClientMeta["hostname"],
		"address":    g.ClientMeta["address"],
		"agent":      g.ClientMeta["agent"],
		"expires_at": g.ExpiresAt.UTC(),
	})
}

// handleCLIDecide records approval or denial.
func (s *Server) handleCLIDecide(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(w, r) {
		return
	}
	user, ok := s.sessionUser(r)
	if !ok {
		jsonError(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if !s.rateLimited(w, r) {
		return
	}
	var req cliDecideRequest
	if !decode(w, r, &req) {
		return
	}

	if req.Approve {
		if req.Org == "" {
			jsonError(w, "pick an organisation", http.StatusBadRequest)
			return
		}
		// Membership is the gate for approving into an organisation. The role
		// travels later, read fresh at poll time.
		if _, err := s.db.RoleIn(r.Context(), user.ID, req.Org); err != nil {
			// Attempts are charged for a bad org exactly as for a bad code:
			// this is the other half of the guessing surface.
			s.db.ChargeCLIGrantAttempt(r.Context(), req.UserCode)
			jsonError(w, "you are not a member of that organisation", http.StatusForbidden)
			return
		}
	}

	err := s.db.DecideCLIGrant(r.Context(), req.UserCode, user.ID, req.Org, req.Approve)
	if errors.Is(err, store.ErrCLIGrantUnusable) {
		jsonError(w, "that code is not waiting for approval", http.StatusNotFound)
		return
	}
	if err != nil {
		s.log.Error("decide cli grant", "err", err)
		jsonError(w, "could not record the decision", http.StatusInternalServerError)
		return
	}

	action := "cli_grant_denied"
	if req.Approve {
		action = "cli_grant_approved"
	}
	s.db.LogAction(r.Context(), req.Org, user.ID, user.Email, action,
		map[string]any{"user_code": req.UserCode})
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// validCallerName mirrors the store grammar tide and the console apply to
// caller names. Display-deep only here — the console re-checks the caller
// against its registry — but a name that fails this could not enrol anywhere.
func validCallerName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
}
