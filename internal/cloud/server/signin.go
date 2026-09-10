package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/analytics"
	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Cookie names. The session and the half-finished login are separate cookies
// backed by separate tables.
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
// It never returns a session. Only a second factor creates one; see
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
		// Hash anyway: argon2id takes tens of milliseconds, so skipping it
		// separates a known address from an unknown one by the clock.
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
		// An OAuth-only account. Same work, same answer, so this route does not
		// disclose whether an address has a password.
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

	// An unverified address cannot hold a session, so no downstream code sees a
	// half-authenticated account.
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

	// Past the password already, so naming the next step discloses nothing.
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
// password and an unreadable hash.
func (s *Server) refuseSignIn(w http.ResponseWriter) {
	jsonError(w, "that email address and password do not match", http.StatusUnauthorized)
}

type verifyRequest struct {
	Code string `json:"code"`
}

// handleVerifySecondFactor completes a sign-in.
//
// Accepts either a TOTP code or a backup code, told apart by shape rather than
// by a field the client sets: a six-digit string is TOTP, anything else is
// tried as a backup code.
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
		// The pending login survives a wrong code; the rate limiter bounds
		// guessing.
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
// Both callers must do this before issuing a session; see handleEnrolFinish.
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
	// The pending login just spent must be the one the factor was checked
	// against. On the enrolment path userID comes from requireEnrolable, which
	// prefers a live session, so a browser holding one account's session and
	// another's pending cookie enrols a factor on the first while spending the
	// second.
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

	s.capture(analytics.EventSignedIn, userID, "", "", nil)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Signed in."})
}

// handleEnrolBegin mints a secret and returns what an authenticator needs.
//
// Reachable from a pending login with may_enrol set, which is a first factor,
// and from an authenticated session, which is a replacement.
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
	// Stored unconfirmed. A page closed mid-enrolment would otherwise leave the
	// account gated behind a factor no authenticator holds.
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

	// Claimed before ConfirmTOTP and ReplaceBackupCodes write. Claiming after
	// them loses the plaintext backup codes: the claim fails, the response says
	// the sign-in expired, and the account keeps a confirmed factor and ten
	// hashes whose plaintext was never returned.
	//
	// The reachable case is a live session with a stale pending cookie.
	// requireEnrolable prefers the session and never reads the pending login,
	// so only the spend fails, and re-enrolment has already replaced the
	// existing backup codes.
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

	s.capture(analytics.EventSecondFactor, userID, "", "", nil)

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

	// Last, so the window between storing the hashes and returning the
	// plaintext is as small as possible. A dropped response still loses the
	// codes; regenerating them is a route for that reason.
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
		// A field, not something to read out of the message. The route has two
		// exits and the session cookie that distinguishes them is HttpOnly.
		"signed_in": completing,
	})
}

// requirePending resolves a half-finished login.
//
// wantEnrol asks for one allowed to enrol. A pending login for an account that
// already holds a factor cannot reach enrolment, which would let a password
// alone replace that factor.
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
// Unauthenticated, so it carries the names of the configured OAuth providers
// and nothing account-shaped.
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	// providerNames() builds with make(), so this serialises as `[]` and never
	// `null` when no provider is configured. The page branches on length.
	// TestAuthConfigReportsNoProvidersAsAnEmptyList pins the wire format.
	providers := s.providerNames()
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": providers,
	})
}

// handlePendingState reports whether a half-finished sign-in is in progress.
//
// It answers only for the pending cookie the caller already holds, and returns
// the same `next` value handleLogin computed when it set that cookie. Holding
// the cookie means the password has already been accepted.
//
// It returns neither the email address nor the user id; the page renders
// without them.
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
	// same way.
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
	s.endSession(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Signed out."})
}

// handleEndSession ends a session and sends the browser to sign-in.
//
// A console signs somebody out by clearing its own cookie and then sending the
// browser here, which is the only way its Cloud session ends: the two are
// separate origins, and a console can clear no cookie but its own. This is
// OIDC's end_session_endpoint, and GET because a browser is redirected to it.
//
// It takes no post-logout redirect parameter. The specification requires a
// provider to redirect only to values it recognises, and accepting none removes
// what that check exists for; /signin is where a browser with no session
// belongs.
//
// A GET that ends a session can be triggered by any page able to make a browser
// fetch a URL. What that produces is an unwanted sign-out, with no parameters to
// influence and nothing disclosed.
//
// Another console already signed in keeps its own cookie until it expires. Only
// minting a new assertion needs the Cloud session, so this stops the next
// sign-in everywhere and ends no session but its own and the caller's.
func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	http.Redirect(w, r, "/signin", http.StatusSeeOther)
}

// endSession deletes the session behind the request's cookie and clears both
// cookies. Does the same work whether or not a session existed.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.db.DeleteSession(r.Context(), c.Value); err != nil {
			s.log.Error("delete session", "err", err)
		}
	}
	s.clearCookie(w, sessionCookie)
	s.clearCookie(w, pendingCookie)
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
