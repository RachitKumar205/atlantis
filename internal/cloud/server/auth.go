package server

import (
	"context"
	"errors"
	"html"
	"net/http"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The one answer sign-up and reset-request give, whatever happened.
//
// # Why every outcome shares a response
//
// Both routes take an email address chosen by whoever is asking. If the answer
// differs between "this address has an account" and "it does not", the route is
// a membership oracle: point it at a list and learn who has one. That matters
// more here than for most products, because an Atlantis Cloud account is an
// account that can reach production databases.
//
// So the body is fixed, the status is fixed, and — see floorLatency — the time
// is fixed too. The body alone is not enough: the registered path writes a row,
// mints a token and sends an email, and the unregistered one does none of it.
const checkYourEmail = "If that address can receive mail, a message is on its way."

type signupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// handleSignup creates an account and sends a verification link.
//
// An address that already has an account gets a message too — one saying
// somebody tried to sign up with it — rather than being told it exists. That
// keeps the two paths indistinguishable to the requester while still telling
// the person who actually owns the address that something happened.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { floorLatency(r.Context(), start, s.sleep) }()

	if !s.rateLimited(w, r) {
		return
	}

	var req signupRequest
	if !decode(w, r, &req) {
		return
	}
	email := store.NormalizeEmail(req.Email)

	// Shape errors are reported plainly. They are not an oracle: they say
	// something about the string that was sent, not about whether it has an
	// account.
	if !validEmail(email) {
		jsonError(w, "that does not look like an email address", http.StatusBadRequest)
		return
	}
	if err := authn.Strength(req.Password, email, req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.breached(r.Context(), req.Password) {
		jsonError(w, "that password has appeared in a data breach; please choose another",
			http.StatusBadRequest)
		return
	}

	hash, err := authn.Hash(req.Password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		jsonError(w, "could not create the account", http.StatusInternalServerError)
		return
	}

	user, err := s.db.CreateUser(r.Context(), email, req.Name, &hash)
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		// Tell the owner of the address, not the requester.
		s.send(r.Context(), email, "Someone tried to sign up with your address",
			"Somebody entered this address on the Atlantis Cloud sign-up form.\n\n"+
				"If that was you, you already have an account — sign in instead, or "+
				"reset your password if you have forgotten it.\n\n"+
				"If it was not you, nothing has changed and you can ignore this.")
		writeJSON(w, http.StatusOK, map[string]string{"message": checkYourEmail})
		return
	case err != nil:
		s.log.Error("create account", "err", err)
		jsonError(w, "could not create the account", http.StatusInternalServerError)
		return
	}

	token, err := s.db.IssueEmailToken(r.Context(), user.ID, user.Email,
		store.PurposeVerifyEmail, store.VerifyTokenTTL)
	if err != nil {
		s.log.Error("issue verification token", "err", err)
		jsonError(w, "could not create the account", http.StatusInternalServerError)
		return
	}

	s.send(r.Context(), user.Email, "Verify your email address",
		"Open this link to finish setting up your Atlantis Cloud account:\n\n"+
			s.link("/verify", token)+"\n\nThe link is good for 24 hours.")

	writeJSON(w, http.StatusOK, map[string]string{"message": checkYourEmail})
}

type resetRequest struct {
	Email string `json:"email"`
}

// handleResetRequest sends a password-reset link, if there is an account.
//
// Answers identically either way. See checkYourEmail.
func (s *Server) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { floorLatency(r.Context(), start, s.sleep) }()

	if !s.rateLimited(w, r) {
		return
	}

	var req resetRequest
	if !decode(w, r, &req) {
		return
	}
	email := store.NormalizeEmail(req.Email)

	// The answer is the same for a malformed address as for a valid one with no
	// account. Reporting "that is not an address" here would be harmless, but
	// it is one more branch a prober can measure, and there is nothing to gain.
	if validEmail(email) {
		s.issueReset(r.Context(), email)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": checkYourEmail})
}

// issueReset does the work when the address is real. Failures are logged and
// swallowed: reporting them would distinguish the paths this route exists to
// keep identical.
func (s *Server) issueReset(ctx context.Context, email string) {
	user, err := s.db.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.log.Error("look up account for reset", "err", err)
		return
	}

	token, err := s.db.IssueEmailToken(ctx, user.ID, user.Email,
		store.PurposeResetPassword, store.ResetTokenTTL)
	if err != nil {
		s.log.Error("issue reset token", "err", err)
		return
	}

	s.send(ctx, user.Email, "Reset your password",
		"Open this link to choose a new password for your Atlantis Cloud account:\n\n"+
			s.link("/reset", token)+"\n\nThe link is good for one hour and can be used once.\n\n"+
			"If you did not ask for this, nothing has changed and you can ignore it.")
}

type resetComplete struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// handleResetComplete sets a new password, for a JSON client.
func (s *Server) handleResetComplete(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	var req resetComplete
	if !decode(w, r, &req) {
		return
	}

	msg, code := s.completeReset(r.Context(), req.Token, req.Password)
	if code != http.StatusOK {
		jsonError(w, msg, code)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// handleResetSubmit is the same thing from the emailed form.
//
// The form posts here rather than to the JSON route because an ordinary HTML
// submission sends form encoding, and the pages this server serves carry no
// JavaScript — the Content-Security-Policy allows none. Both routes call
// completeReset, so there is one implementation of what a reset does and no
// chance of the two drifting.
func (s *Server) handleResetSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		page(w, http.StatusBadRequest, "That form could not be read. Please try again.")
		return
	}

	msg, code := s.completeReset(r.Context(), r.PostFormValue("token"), r.PostFormValue("password"))
	page(w, code, msg)
}

// completeReset spends the token and sets the password.
//
// Returns a message and a status rather than writing a response, because two
// callers render it differently — one as JSON, one as a page for a browser.
//
// It does not sign anybody in, and that is the design rather than an omission:
// signing in needs a second factor, and proving control of a mailbox is one
// factor. A reset that produced a session would make the mailbox sufficient on
// its own, which is the whole thing two factors exist to prevent.
func (s *Server) completeReset(ctx context.Context, token, password string) (string, int) {
	// The token is checked BEFORE the password is judged. The other order tells
	// somebody holding an invalid token whether their proposed password would
	// have been acceptable, which is a small oracle and a free one to close.
	spent, err := s.db.SpendEmailToken(ctx, token, store.PurposeResetPassword)
	if errors.Is(err, store.ErrTokenInvalid) {
		return "This link is not valid any more — request another.", http.StatusBadRequest
	}
	if err != nil {
		s.log.Error("spend reset token", "err", err)
		return "Could not reset the password.", http.StatusInternalServerError
	}

	// The token is spent by this point, and every rejection below leaves it
	// spent. Deliberate: a token that survived a rejected password would let
	// somebody grind candidate passwords against the strength check with a
	// single link.
	const used = " The link has been used, so please request another."

	if err := authn.Strength(password, spent.Email); err != nil {
		return err.Error() + "." + used, http.StatusBadRequest
	}
	if s.breached(ctx, password) {
		return "That password has appeared in a data breach; please choose another." + used,
			http.StatusBadRequest
	}

	hash, err := authn.Hash(password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		return "Could not reset the password.", http.StatusInternalServerError
	}
	if err := s.db.SetPassword(ctx, spent.UserID, hash); err != nil {
		s.log.Error("set password", "err", err)
		return "Could not reset the password.", http.StatusInternalServerError
	}

	// Every other reset link for this account stops working. Without this,
	// asking three times leaves three live credentials and using one leaves
	// two — which matters precisely when the reset was requested because
	// somebody else had access.
	if err := s.db.InvalidateEmailTokens(ctx, spent.UserID, store.PurposeResetPassword); err != nil {
		s.log.Error("invalidate other reset tokens", "user", spent.UserID, "err", err)
	}

	return "Your password has been changed. You can now sign in.", http.StatusOK
}

// handleVerify spends a verification token.
//
// Plain text, because a person reaches this from an email in a browser. The
// Cloud sign-in app replaces it with something designed.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	spent, err := s.db.SpendEmailToken(r.Context(), token, store.PurposeVerifyEmail)
	if err != nil {
		page(w, http.StatusBadRequest,
			"This link is not valid any more.\n\nVerification links last 24 hours and "+
				"can be used once. Sign in to send yourself another.")
		return
	}
	if err := s.db.MarkEmailVerified(r.Context(), spent.UserID); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		// ErrNotFound here means it was already verified, which is not a
		// failure — somebody opened the link twice, or from two devices.
		s.log.Error("mark verified", "user", spent.UserID, "err", err)
		page(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}

	page(w, http.StatusOK, "Your email address is verified.\n\nYou can close this page.")
}

// handleResetForm renders somewhere to type a new password.
//
// Unstyled and script-free on purpose: the strict Content-Security-Policy this
// server sets allows nothing to load, and the form posts to the JSON endpoint
// through an ordinary submission. The Cloud sign-in app replaces this; until it
// exists, a reset email that leads somewhere usable is worth more than one that
// leads to a page nobody has built.
func (s *Server) handleResetForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Escaped. The token is attacker-supplied and is being placed into an
	// attribute, which is the textbook injection point.
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8">` +
		`<title>Choose a new password</title>` +
		`<h1>Choose a new password</h1>` +
		`<form method="post" action="/reset">` +
		`<input type="hidden" name="token" value="` + html.EscapeString(token) + `">` +
		`<label>New password <input type="password" name="password" autocomplete="new-password" required></label> ` +
		`<button type="submit">Change password</button>` +
		`</form>`))
}

// page writes a plain-text response.
//
// Every caller is a page reached with a single-use credential in the query
// string — a verification token, a reset token, an OAuth authorization code —
// so none of them may be stored. Referrer-Policy already stops the URL leaking
// sideways; this stops it being written down.
func page(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body + "\n"))
}

// ── Shared plumbing ─────────────────────────────────────────────────────────

// rateLimited reports whether the request may proceed, answering it if not.
func (s *Server) rateLimited(w http.ResponseWriter, r *http.Request) bool {
	ok, retry := s.lim.allow(s.clientIP(r))
	if ok {
		return true
	}
	w.Header().Set("Retry-After", itoa(retry))
	jsonError(w, "too many requests — try again shortly", http.StatusTooManyRequests)
	return false
}

// breached reports whether a password is known to have leaked.
//
// Fails OPEN: a corpus we cannot reach means the password is accepted. That is
// the policy decision the authn package deliberately leaves to the caller —
// sign-up must not stop working because a third party is down. The log line is
// what makes the gap visible rather than silent.
func (s *Server) breached(ctx context.Context, password string) bool {
	yes, err := s.breach.Breached(ctx, password)
	if err != nil {
		s.log.Warn("could not check the breach corpus; accepting the password unchecked", "err", err)
		return false
	}
	return yes
}

// send delivers a message, logging rather than returning a failure.
//
// Callers are the routes that must answer identically whatever happened, so a
// send failure cannot reach the response — it would distinguish an address that
// exists from one that does not.
func (s *Server) send(ctx context.Context, to, subject, body string) {
	if err := s.mailer.Send(ctx, to, subject, body); err != nil {
		s.log.Error("send mail", "subject", subject, "err", err)
	}
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
