package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Cookie names. The session and the half-finished login are separate cookies
// for the same reason they are separate tables: nothing should be able to treat
// one as the other by forgetting a field.
const (
	sessionCookie = "atl_cloud_session"
	pendingCookie = "atl_cloud_pending"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin verifies a password and hands back a half-finished login.
//
// It never returns a session. The only thing that creates one is a second
// factor, which is the property the two-table split makes structural — see
// migrations/cloud/0003 and store.CreatePendingLogin.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { floorLatency(r.Context(), start, s.sleep) }()

	if !s.rateLimited(w, r) {
		return
	}
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	email := store.NormalizeEmail(req.Email)

	user, err := s.db.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		// Hash anyway. Skipping it here is the enumeration oracle: argon2id
		// takes tens of milliseconds, the response body is identical either
		// way, and the clock is not.
		authn.VerifyDecoy(req.Password)
		s.refuseSignIn(w)
		return
	}
	if err != nil {
		s.log.Error("look up account", "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return
	}

	if !user.HasPassword() {
		// An OAuth-only account. Same work, same answer: whether an address has
		// a password is not something this route should disclose.
		authn.VerifyDecoy(req.Password)
		s.refuseSignIn(w)
		return
	}

	ok, needsRehash, err := authn.Verify(req.Password, *user.PasswordHash)
	if err != nil {
		// A hash this build cannot read. Distinct from a wrong password, and
		// logged as such — otherwise an account with a corrupt row looks to
		// everyone like a user who has forgotten their password.
		s.log.Error("stored password hash is unreadable", "user", user.ID, "err", err)
		s.refuseSignIn(w)
		return
	}
	if !ok {
		s.refuseSignIn(w)
		return
	}

	// Cost parameters have moved on since this hash was written. Rewriting it
	// needs the plaintext, and this is the only moment it exists.
	if needsRehash {
		if fresh, err := authn.Hash(req.Password); err == nil {
			if err := s.db.SetPassword(r.Context(), user.ID, fresh); err != nil {
				s.log.Error("rehash password", "user", user.ID, "err", err)
			}
		}
	}

	// An unverified address cannot hold a session, so there is no
	// half-authenticated account to reason about anywhere downstream.
	if user.EmailVerifiedAt == nil {
		jsonError(w, "verify your email address first — check your inbox, "+
			"or request another link", http.StatusForbidden)
		return
	}

	enrolled, err := s.db.ForUser(user.ID).HasConfirmedFactor(r.Context())
	if err != nil {
		s.log.Error("check second factor", "user", user.ID, "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return
	}

	token, err := s.db.CreatePendingLogin(r.Context(), user.ID, !enrolled)
	if err != nil {
		s.log.Error("create pending login", "user", user.ID, "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	s.setCookie(w, pendingCookie, token, store.PendingTTL)

	// The account is told which of the two things to do next. That is not a
	// disclosure — it is already past the password.
	next := "verify"
	if !enrolled {
		next = "enrol"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"next":    next,
		"message": "Password accepted. One more step.",
	})
}

// refuseSignIn is the one answer a failed password gets.
//
// Identical for an unknown address, an account with no password, a wrong
// password and an unreadable hash. Any difference between them is a way to
// learn which addresses have accounts.
func (s *Server) refuseSignIn(w http.ResponseWriter) {
	jsonError(w, "that email address and password do not match", http.StatusUnauthorized)
}

// ── The second factor ───────────────────────────────────────────────────────

type verifyRequest struct {
	Code string `json:"code"`
}

// handleVerifySecondFactor completes a sign-in.
//
// Accepts either a TOTP code or a backup code. The two are told apart by shape
// rather than by a field the client sets: a six-digit string is a TOTP code and
// anything else is tried as a backup code. A client that had to declare which
// would be a client that could declare the wrong one.
func (s *Server) handleVerifySecondFactor(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	pending, ok := s.requirePending(w, r, false)
	if !ok {
		return
	}
	var req verifyRequest
	if !decode(w, r, &req) {
		return
	}

	accepted, err := s.checkSecondFactor(r.Context(), pending.UserID, req.Code)
	if err != nil {
		s.log.Error("check second factor", "user", pending.UserID, "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	if !accepted {
		// The pending login SURVIVES a wrong code. Spending it would mean a
		// mistyped digit costs the user their password entry too, and the rate
		// limiter is what bounds guessing.
		jsonError(w, "that code is not right", http.StatusUnauthorized)
		return
	}

	s.completeSignIn(w, r, pending.UserID)
}

// checkSecondFactor accepts a TOTP code or a backup code.
func (s *Server) checkSecondFactor(ctx context.Context, userID, code string) (bool, error) {
	us := s.db.ForUser(userID)

	if isTOTPShaped(code) {
		ciphertext, confirmed, err := us.TOTPSecret(ctx)
		if errors.Is(err, store.ErrNoSecondFactor) || (err == nil && !confirmed) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		secret, err := s.keys.Decrypt(ciphertext, []byte(userID))
		if err != nil {
			return false, err
		}
		step, err := authn.VerifyTOTP(string(secret), code, time.Now())
		if err != nil {
			return false, nil
		}
		// The step is spent, so the same code cannot be presented again inside
		// its own window — which is up to ninety seconds once skew is allowed.
		return us.SpendTOTPStep(ctx, step)
	}

	return s.checkBackupCode(ctx, us, userID, code)
}

func (s *Server) checkBackupCode(ctx context.Context, us *store.UserStore, userID, code string) (bool, error) {
	normalised := authn.NormalizeBackupCode(code)
	if normalised == "" {
		return false, nil
	}

	hashes, err := us.UnusedBackupCodeHashes(ctx)
	if err != nil {
		return false, err
	}
	// Every remaining hash is checked even after one matches, so the time taken
	// does not reveal which code was used or how many are left.
	matched := int64(-1)
	for id, h := range hashes {
		ok, _, err := authn.Verify(normalised, h)
		if err != nil {
			s.log.Error("unreadable backup-code hash", "user", userID, "id", id, "err", err)
			continue
		}
		if ok {
			matched = id
		}
	}
	if matched < 0 {
		return false, nil
	}
	return us.SpendBackupCode(ctx, matched)
}

// isTOTPShaped reports whether a string looks like a six-digit code.
func isTOTPShaped(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// spendPendingLogin claims the half-finished login and proves it belongs to
// userID. It answers the request and returns false on every failure.
//
// Extracted because both callers must do exactly this, and one of them used to
// do it too late — see handleEnrolFinish.
func (s *Server) spendPendingLogin(w http.ResponseWriter, r *http.Request, userID string) bool {
	pendingToken, _ := r.Cookie(pendingCookie)
	if pendingToken == nil {
		jsonError(w, "your sign-in has expired — start again", http.StatusUnauthorized)
		return false
	}

	// Spent here, in one statement, so two requests presenting the same pending
	// login cannot both reach a session.
	spentUser, err := s.db.SpendPendingLogin(r.Context(), pendingToken.Value)
	if errors.Is(err, store.ErrNoSession) {
		jsonError(w, "your sign-in has expired — start again", http.StatusUnauthorized)
		return false
	}
	if err != nil {
		s.log.Error("spend pending login", "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return false
	}
	// Belt and braces: the pending login just spent must be the one the factor
	// was checked against. Only reachable if two requests interleaved, and a
	// mismatch here would mean issuing a session for the wrong account.
	//
	// It matters more on the enrolment path, where userID comes from
	// requireEnrolable and prefers a live SESSION over the pending login: a
	// browser holding one account's session and another's pending cookie would
	// otherwise enrol a factor on the first while spending the second.
	if subtle.ConstantTimeCompare([]byte(spentUser), []byte(userID)) != 1 {
		s.log.Error("pending login changed account mid-request",
			"checked", userID, "spent", spentUser)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return false
	}
	return true
}

// completeSignIn spends the pending login and issues the session.
func (s *Server) completeSignIn(w http.ResponseWriter, r *http.Request, userID string) {
	if !s.spendPendingLogin(w, r, userID) {
		return
	}

	token, err := s.db.CreateSession(r.Context(), userID)
	if err != nil {
		s.log.Error("create session", "user", userID, "err", err)
		jsonError(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	s.clearCookie(w, pendingCookie)
	s.setCookie(w, sessionCookie, token, store.SessionTTL)

	writeJSON(w, http.StatusOK, map[string]string{"message": "Signed in."})
}

// ── Enrolment ───────────────────────────────────────────────────────────────

// handleEnrolBegin mints a secret and returns what an authenticator needs.
//
// Reachable from a pending login with may_enrol set, and from an authenticated
// session — the first is how an account with no factor gets one, the second is
// how somebody replaces theirs.
func (s *Server) handleEnrolBegin(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.requireEnrolable(w, r)
	if !ok {
		return
	}

	user, err := s.db.UserByID(r.Context(), userID)
	if err != nil {
		s.log.Error("look up account", "err", err)
		jsonError(w, "could not start enrolment", http.StatusInternalServerError)
		return
	}

	secret, uri, err := authn.NewTOTPSecret("Atlantis Cloud", user.Email)
	if err != nil {
		s.log.Error("generate TOTP secret", "err", err)
		jsonError(w, "could not start enrolment", http.StatusInternalServerError)
		return
	}

	// Sealed against the user id, so a ciphertext lifted onto another row will
	// not open. See internal/secrets.
	ciphertext, err := s.keys.Encrypt([]byte(secret), []byte(userID))
	if err != nil {
		s.log.Error("seal TOTP secret", "err", err)
		jsonError(w, "could not start enrolment", http.StatusInternalServerError)
		return
	}
	// Stored unconfirmed. Enrolment is not finished until a code proves the
	// authenticator actually captured it — otherwise somebody who closed the
	// page mid-way is locked out by a factor they never had.
	if err := s.db.ForUser(userID).PutTOTPSecret(r.Context(), ciphertext); err != nil {
		s.log.Error("store TOTP secret", "err", err)
		jsonError(w, "could not start enrolment", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret,
		"uri":    uri,
	})
}

// handleEnrolFinish confirms enrolment and issues backup codes.
func (s *Server) handleEnrolFinish(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimited(w, r) {
		return
	}
	userID, ok := s.requireEnrolable(w, r)
	if !ok {
		return
	}
	var req verifyRequest
	if !decode(w, r, &req) {
		return
	}

	us := s.db.ForUser(userID)
	ciphertext, _, err := us.TOTPSecret(r.Context())
	if errors.Is(err, store.ErrNoSecondFactor) {
		jsonError(w, "start enrolment first", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.log.Error("read TOTP secret", "err", err)
		jsonError(w, "could not finish enrolment", http.StatusInternalServerError)
		return
	}
	secret, err := s.keys.Decrypt(ciphertext, []byte(userID))
	if err != nil {
		s.log.Error("open TOTP secret", "user", userID, "err", err)
		jsonError(w, "could not finish enrolment", http.StatusInternalServerError)
		return
	}

	step, err := authn.VerifyTOTP(string(secret), req.Code, time.Now())
	if err != nil {
		jsonError(w, "that code is not right — check the time on your phone "+
			"and try the next one", http.StatusBadRequest)
		return
	}

	// ── Order matters here, and it used to be wrong ─────────────────────────
	//
	// Enrolling during a sign-in completes it; enrolling from a session that
	// already exists does not need to. Either way the pending login is claimed
	// FIRST, before anything is committed.
	//
	// It used to be claimed last, after ConfirmTOTP and ReplaceBackupCodes had
	// both written — so when the claim failed, this handler answered "your
	// sign-in has expired" and dropped the `codes` slice holding the only copy
	// of the plaintext. The account kept a confirmed factor and ten
	// backup-code hashes nobody had ever seen.
	//
	// The reachable shape is NOT a pending login that merely timed out. That
	// case never gets here: requireEnrolable resolves it through
	// requirePending, which refuses an expired one before any of this runs.
	//
	// It is a live SESSION plus a stale pending cookie. requireEnrolable
	// prefers the session and never looks at the pending login, so the handler
	// runs to completion and only the spend at the end fails. A browser gets
	// into that state by starting a second sign-in and abandoning it, which is
	// also why the account-settings enrolment path is the one that suffers:
	// re-enrolling replaces the existing backup codes, so the failure does not
	// just withhold new codes, it destroys working ones.
	completing := false
	if _, err := r.Cookie(pendingCookie); err == nil {
		if !s.spendPendingLogin(w, r, userID) {
			return
		}
		completing = true
	}

	// Confirmed with the step recorded, so the proving code cannot immediately
	// be replayed as a sign-in.
	if err := us.ConfirmTOTP(r.Context(), step); err != nil {
		s.log.Error("confirm TOTP", "err", err)
		jsonError(w, "could not finish enrolment", http.StatusInternalServerError)
		return
	}

	if completing {
		token, err := s.db.CreateSession(r.Context(), userID)
		if err != nil {
			s.log.Error("create session", "user", userID, "err", err)
			jsonError(w, "could not sign in", http.StatusInternalServerError)
			return
		}
		s.clearCookie(w, pendingCookie)
		s.setCookie(w, sessionCookie, token, store.SessionTTL)
	}

	// Last, so the gap between storing the hashes and handing back the
	// plaintext is as small as it can be made. It cannot be closed: any
	// shown-once secret has a window where the server has written it and the
	// client has not received it — a dropped response is enough. What makes
	// that survivable is being able to mint a fresh set, which is why
	// regenerating backup codes is a route and not a support ticket.
	codes, hashes, err := authn.NewBackupCodes()
	if err != nil {
		s.log.Error("generate backup codes", "err", err)
		jsonError(w, "could not finish enrolment", http.StatusInternalServerError)
		return
	}
	if err := us.ReplaceBackupCodes(r.Context(), hashes); err != nil {
		s.log.Error("store backup codes", "err", err)
		jsonError(w, "could not finish enrolment", http.StatusInternalServerError)
		return
	}

	message := "Two-factor authentication is on. Save these codes somewhere safe — they are shown once."
	if completing {
		message = "Two-factor authentication is on and you are signed in. Save these codes somewhere safe — they are shown once."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backup_codes": codes,
		"message":      message,
		// Stated as a field rather than left to be read out of the message.
		// This route has two exits — enrolling during a sign-in completes it,
		// enrolling from a session does not — and the client has to know which
		// one it took. The session cookie that would otherwise say so is
		// HttpOnly, so the alternative was matching on English prose.
		"signed_in": completing,
	})
}

// ── Session plumbing ────────────────────────────────────────────────────────

// requirePending resolves a half-finished login.
//
// wantEnrol asks for one that is allowed to enrol. A pending login for an
// account that already has a factor cannot reach enrolment — otherwise somebody
// holding a password could replace the second factor with one of their own,
// which is the whole gate walked around.
func (s *Server) requirePending(w http.ResponseWriter, r *http.Request, wantEnrol bool) (*store.PendingLogin, bool) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		jsonError(w, "start by signing in", http.StatusUnauthorized)
		return nil, false
	}
	p, err := s.db.PendingLoginFor(r.Context(), c.Value)
	if err != nil {
		jsonError(w, "your sign-in has expired — start again", http.StatusUnauthorized)
		return nil, false
	}
	if wantEnrol && !p.MayEnrol {
		jsonError(w, "this account already has two-factor authentication", http.StatusForbidden)
		return nil, false
	}
	return p, true
}

// requireEnrolable resolves whoever may set up a second factor: a pending login
// that is allowed to, or an existing session.
func (s *Server) requireEnrolable(w http.ResponseWriter, r *http.Request) (string, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if u, err := s.db.SessionUser(r.Context(), c.Value); err == nil {
			return u.ID, true
		}
	}
	p, ok := s.requirePending(w, r, true)
	if !ok {
		return "", false
	}
	return p.UserID, true
}

// handleAuthConfig reports what the sign-in screen has to know up front.
//
// Unauthenticated, and deliberately thin: the names of the configured OAuth
// providers and nothing else. Anything account-shaped added here would be a
// disclosure to an anonymous caller.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	// providerNames() builds with make(), so this is `[]` and never `null` on
	// a deployment with no providers configured. That distinction is load
	// bearing — the page branches on length, and `null` is not a length — so
	// TestAuthConfigReportsNoProvidersAsAnEmptyList asserts the wire format
	// rather than trusting the constructor to stay that way.
	//
	// A `if providers == nil` normalisation stood here first and was removed:
	// mutation testing showed nothing could make it fire.
	providers := s.providerNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": providers,
	})
}

// handlePendingState reports whether a half-finished sign-in is in progress.
//
// # Why this is not a disclosure
//
// It answers only for the pending cookie the caller already holds, and says no
// more than handleLogin already said when it set that cookie: `next` is the
// same value, computed the same way. Somebody holding the cookie is past the
// password.
//
// It deliberately does NOT return the email address or the user id. The page
// does not need them to render, and a half-finished login is exactly the state
// where the least should be said.
func (s *Server) handlePendingState(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(pendingCookie)
	if err != nil {
		// Not an error. "No sign-in in progress" is a normal answer, and a 401
		// here would make the app treat a first visit as a failure.
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}
	p, err := s.db.PendingLoginFor(r.Context(), c.Value)
	if err != nil {
		// Expired or already spent. Clear the cookie so the next load does not
		// ask again with the same dead token.
		s.clearCookie(w, pendingCookie)
		writeJSON(w, http.StatusOK, map[string]any{"pending": false})
		return
	}

	// The same value handleLogin returned when it set this cookie, computed the
	// same way. That is what makes a reload continuous rather than a restart.
	next := "verify"
	if p.MayEnrol {
		next = "enrol"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending": true,
		"next":    next,
	})
}

// handleLogout ends a session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.db.DeleteSession(r.Context(), c.Value); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	// Cleared regardless, and answered the same way whether or not there was a
	// session. Signing out twice is a thing browsers do.
	s.clearCookie(w, sessionCookie)
	s.clearCookie(w, pendingCookie)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Signed out."})
}

// handleResendVerification sends another verification link.
//
// Answers identically whether or not the address has an unverified account, for
// the same reason sign-up and reset-request do.
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
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

	if validEmail(email) {
		if user, err := s.db.UserByEmail(r.Context(), email); err == nil && user.EmailVerifiedAt == nil {
			token, err := s.db.IssueEmailToken(r.Context(), user.ID, user.Email,
				store.PurposeVerifyEmail, store.VerifyTokenTTL)
			if err != nil {
				s.log.Error("issue verification token", "err", err)
			} else {
				s.send(r.Context(), user.Email, "Verify your email address",
					"Open this link to finish setting up your Atlantis Cloud account:\n\n"+
						s.link("/verify", token)+"\n\nThe link is good for 24 hours.")
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": checkYourEmail})
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		// Lax rather than Strict: the verification and reset links arrive as
		// top-level navigations from an email client, and Strict would drop the
		// cookie on exactly those.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}
