package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/oauth"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// oauthCookie carries one sign-in attempt's state across the provider.
const oauthCookie = "atl_cloud_oauth"

// oauthTTL bounds how long a started sign-in can be finished. Shorter than the
// pending login it produces.
const oauthTTL = 10 * time.Minute

// Intents. Whether the callback is a sign-in or an attachment to an account
// that already holds a session.
const (
	intentSignIn = "signin"
	intentLink   = "link"
)

// oauthState is the value carried in the OAuth state cookie. It binds a
// callback to a request that started here, naming the provider and the
// operation.
//
// It carries no user id. For a link, the acting account comes from the session
// cookie at callback time, so a forged or replayed state cannot attach a
// provider account to a different Cloud account.
type oauthState struct {
	Provider string
	Intent   string
	State    string
	Verifier string

	// Expires is checked here, not left to the cookie's MaxAge. A copy of the
	// cookie value can be presented after MaxAge has passed, so the deadline
	// has to travel inside the value.
	Expires time.Time
}

// encode packs the state into a cookie value. The separator is safe because no
// field can contain it: both tokens are base64url, the provider and intent are
// constants from this file, and the deadline is decimal.
func (s oauthState) encode() string {
	return strings.Join([]string{
		s.Provider, s.Intent, s.State, s.Verifier,
		strconv.FormatInt(s.Expires.Unix(), 10),
	}, ".")
}

func decodeOAuthState(v string, now time.Time) (oauthState, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 5 {
		return oauthState{}, false
	}
	s := oauthState{Provider: parts[0], Intent: parts[1], State: parts[2], Verifier: parts[3]}
	if s.Provider == "" || s.State == "" || s.Verifier == "" {
		return oauthState{}, false
	}
	if s.Intent != intentSignIn && s.Intent != intentLink {
		return oauthState{}, false
	}
	unix, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return oauthState{}, false
	}
	s.Expires = time.Unix(unix, 0)
	if !now.Before(s.Expires) {
		return oauthState{}, false
	}
	return s, true
}

// redirectURI is where the provider sends the browser back. Built from
// CLOUD_PUBLIC_URL and never from the request: a redirect_uri taken from a
// parameter has the provider deliver the authorization code to whatever host
// was asked for.
func (s *Server) redirectURI(provider string) string {
	return fmt.Sprintf("%s/auth/%s/callback", s.cfg.PublicURL, provider)
}

// handleOAuthStart sends the browser to a provider. Closed over the provider's
// name rather than reading it from the path, so nothing in the request can
// influence which endpoint the authorization code is exchanged at.
func (s *Server) handleOAuthStart(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.oauthStart(w, r, name)
	}
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request, name string) {
	if !s.rateLimited(w, r) {
		return
	}
	p, ok := s.providers[name]
	if !ok {
		page(w, http.StatusNotFound, "That sign-in method is not available here.")
		return
	}

	// A session means this is an attachment, not a sign-in. Read here rather
	// than at the callback so the intent is fixed before the round trip: a
	// session expiring mid-consent must not turn a link into a new account.
	intent := intentSignIn
	if _, signedIn := s.sessionUser(r); signedIn {
		intent = intentLink
	}

	state, verifier, err := oauth.NewState()
	if err != nil {
		s.log.Error("mint oauth state", "err", err)
		page(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
		return
	}
	s.setCookie(w, oauthCookie, oauthState{
		Provider: name, Intent: intent, State: state, Verifier: verifier,
		Expires: time.Now().Add(oauthTTL),
	}.encode(), oauthTTL)

	http.Redirect(w, r,
		p.AuthCodeURL(state, oauth.Challenge(verifier), s.redirectURI(name)),
		http.StatusFound)
}

// handleOAuthCallback finishes a sign-in or a link.
//
// This must not issue a session. Every path through here ends in a pending
// login, which the second factor turns into a session. A provider vouching for
// an identity is one factor.
func (s *Server) handleOAuthCallback(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.oauthCallback(w, r, name)
	}
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request, name string) {
	if !s.rateLimited(w, r) {
		return
	}
	p, ok := s.providers[name]
	if !ok {
		page(w, http.StatusNotFound, "That sign-in method is not available here.")
		return
	}

	c, err := r.Cookie(oauthCookie)
	if err != nil {
		page(w, http.StatusBadRequest,
			"This sign-in did not start here, or it took too long. Please try again.")
		return
	}
	// Cleared before anything else can fail: a cookie surviving a failed
	// callback can be replayed against a second authorization code.
	s.clearCookie(w, oauthCookie)

	st, ok := decodeOAuthState(c.Value, time.Now())
	if !ok {
		page(w, http.StatusBadRequest,
			"That sign-in expired, or it was started in another tab. Please try again.")
		return
	}
	// The cookie names its provider, so a state minted for GitHub cannot be
	// spent at Google's callback.
	if st.Provider != name {
		page(w, http.StatusBadRequest, "This sign-in could not be completed. Please try again.")
		return
	}
	// Constant time: the right-hand side comes from the request and can be
	// varied across attempts.
	if subtle.ConstantTimeCompare([]byte(st.State), []byte(r.URL.Query().Get("state"))) != 1 {
		page(w, http.StatusBadRequest, "This sign-in could not be completed. Please try again.")
		return
	}

	// The provider refused, or consent was declined. Its description is not
	// echoed back: that is third-party text rendered into a page.
	if e := r.URL.Query().Get("error"); e != "" {
		page(w, http.StatusOK, "Sign-in was cancelled. You can close this page.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		page(w, http.StatusBadRequest, "This sign-in could not be completed. Please try again.")
		return
	}

	id, err := p.Identify(r.Context(), code, st.Verifier, s.redirectURI(name))
	if errors.Is(err, oauth.ErrNoVerifiedEmail) {
		page(w, http.StatusForbidden,
			"Your "+providerLabel(name)+" account has no verified email address.\n\n"+
				"Verify one there and try again, or sign up with an email and password.")
		return
	}
	if err != nil {
		s.log.Error("identify at provider", "provider", name, "err", err)
		page(w, http.StatusBadGateway,
			"Could not reach "+providerLabel(name)+". Please try again.")
		return
	}

	if st.Intent == intentLink {
		s.finishLink(w, r, name, id)
		return
	}
	s.finishOAuthSignIn(w, r, name, id)
}

// finishOAuthSignIn resolves a provider identity to an account and starts the
// second leg.
func (s *Server) finishOAuthSignIn(w http.ResponseWriter, r *http.Request, provider string, id *oauth.Identity) {
	ctx := r.Context()

	user, err := s.db.UserByIdentity(ctx, provider, id.Subject)
	switch {
	case err == nil:
		// Known link.
	case errors.Is(err, store.ErrNotFound):
		var ok bool
		user, ok = s.adoptOrCreate(w, r, provider, id)
		if !ok {
			// adoptOrCreate has already written the response.
			return
		}
	default:
		s.log.Error("look up identity", "provider", provider, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return
	}

	s.startSecondLeg(w, r, user, provider)
}

// adoptOrCreate handles a provider account with no existing link.
//
// False means the response is already written, whether the account was refused
// or the lookup failed. The caller must return without writing.
func (s *Server) adoptOrCreate(w http.ResponseWriter, r *http.Request, provider string, id *oauth.Identity) (*store.User, bool) {
	ctx := r.Context()

	existing, err := s.db.UserByEmail(ctx, id.Email)
	if errors.Is(err, store.ErrNotFound) {
		// One transaction, so no state exists where the account is present
		// without the link that reaches it.
		created, err := s.db.CreateUserWithIdentity(ctx,
			id.Email, id.Name, provider, id.Subject, id.Email)
		if errors.Is(err, store.ErrIdentityClaimed) {
			s.refuseClaimed(w, r, provider)
			return nil, false
		}
		if err != nil {
			s.log.Error("create account from provider", "provider", provider, "err", err)
			page(w, http.StatusInternalServerError, "Could not create your account. Please try again.")
			return nil, false
		}
		return created, true
	}
	if err != nil {
		s.log.Error("look up account by email", "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return nil, false
	}

	// Auto-linking to an existing account requires that account to hold a
	// confirmed second factor. This trusts the provider's assertion about an
	// address, so with no factor behind it, registering that address at the
	// provider reaches the account.
	//
	// The unverified case is answered first: the factorless refusal directs
	// people to their password, and handleLogin rejects an unverified address.
	//
	// Nothing here sends mail; the address comes from the provider account.
	if existing.EmailVerifiedAt == nil {
		page(w, http.StatusConflict,
			"That address already has an Atlantis account whose email is not "+
				"verified yet.\n\n"+
				"Open the verification link that was sent to it, or request another "+
				"with POST /api/auth/verify/resend, then try again.")
		return nil, false
	}

	enrolled, err := s.db.ForUser(existing.ID).HasConfirmedFactor(ctx)
	if err != nil {
		s.log.Error("check second factor", "user", existing.ID, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return nil, false
	}
	// Refused before anything is written. The branch below computes mayEnrol
	// from `enrolled`, so a factorless account reaching it returns an ENROLLING
	// pending login for an account the provider holder does not own.
	if !enrolled {
		page(w, http.StatusConflict,
			"That address already has an Atlantis account with no two-factor "+
				"authentication set up yet.\n\n"+
				"Sign in with your password to finish setting it up. You can connect "+
				providerLabel(provider)+" afterwards.")
		return nil, false
	}

	if err := s.db.LinkIdentity(ctx, existing.ID, provider, id.Subject, id.Email); err != nil {
		if errors.Is(err, store.ErrIdentityClaimed) {
			s.refuseClaimed(w, r, provider)
			return nil, false
		}
		s.log.Error("link identity", "provider", provider, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return nil, false
	}
	return existing, true
}

// startSecondLeg issues the pending login and tells the browser what is next.
//
// The same two calls handleLogin makes after a correct password, in the same
// order, so one definition covers what a first factor earns.
func (s *Server) startSecondLeg(w http.ResponseWriter, r *http.Request, user *store.User, provider string) {
	// Any session already in this browser belongs to whoever used it last, and
	// it is not the account being signed in to now. Left in place it outranks
	// the pending login at requireEnrolable, which prefers a session, so
	// enrolment would set a factor on the wrong account and hand back a session
	// for it. A new sign-in ends the previous session.
	s.clearCookie(w, sessionCookie)

	enrolled, err := s.db.ForUser(user.ID).HasConfirmedFactor(r.Context())
	if err != nil {
		s.log.Error("check second factor", "user", user.ID, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return
	}

	token, err := s.db.CreatePendingLogin(r.Context(), user.ID, !enrolled)
	if err != nil {
		s.log.Error("create pending login", "user", user.ID, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return
	}
	s.setCookie(w, pendingCookie, token, store.PendingTTL)

	next := "verify"
	if !enrolled {
		next = "enrol"
	}
	// The raw name, not providerLabel's display form. Every redirect out of
	// this file carries `provider=<id>` and the page renders the label.
	s.handOff(w, r, next, provider)
}

// handOff returns the browser to the sign-in application.
//
// The Location is relative, so it resolves against this origin, which is where
// the application is served from. `next` is a fixed literal in every caller.
func (s *Server) handOff(w http.ResponseWriter, r *http.Request, next, provider string) {
	http.Redirect(w, r, "/signin?next="+next+"&provider="+url.QueryEscape(provider),
		http.StatusSeeOther)
}

// refuseClaimed sends a sign-in back to the application with the reason.
//
// The link path has its own version, backToAccount with "claimed", so a
// provider connected from account settings returns there rather than to the
// sign-in screen.
func (s *Server) refuseClaimed(w http.ResponseWriter, r *http.Request, provider string) {
	http.Redirect(w, r,
		"/signin?error=claimed&provider="+url.QueryEscape(provider),
		http.StatusSeeOther)
}

// finishLink attaches a provider account to the signed-in account.
//
// The user comes from the session, not from the state cookie. This is the
// branch a forged state would otherwise use to choose whose account gains a new
// way in, which is why oauthState carries no identity.
func (s *Server) finishLink(w http.ResponseWriter, r *http.Request, provider string, id *oauth.Identity) {
	user, ok := s.sessionUser(r)
	if !ok {
		// The session went away during the round trip.
		page(w, http.StatusUnauthorized,
			"You were signed out while connecting "+providerLabel(provider)+
				". Sign in and try again.")
		return
	}

	if err := s.db.LinkIdentity(r.Context(), user.ID, provider, id.Subject, id.Email); err != nil {
		if errors.Is(err, store.ErrIdentityClaimed) {
			// The account screen, where the link was started, not sign-in.
			s.backToAccount(w, r, provider, "claimed")
			return
		}
		s.log.Error("link identity", "provider", provider, "err", err)
		s.backToAccount(w, r, provider, "error")
		return
	}
	s.backToAccount(w, r, provider, "connected")
}

// backToAccount returns the browser to the account screen after a link attempt.
//
// The outcome travels as a query parameter the application reads, so the
// browser stays inside it.
//
// outcome is a fixed literal at every call site, so the query string carries no
// caller-supplied text.
func (s *Server) backToAccount(w http.ResponseWriter, r *http.Request, provider, outcome string) {
	http.Redirect(w, r,
		"/account?provider="+url.QueryEscape(provider)+"&outcome="+outcome,
		http.StatusSeeOther)
}

// handleListIdentities reports which providers are connected.
func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	linked, err := s.db.IdentitiesOf(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list identities", "err", err)
		jsonError(w, "could not read your connected accounts", http.StatusInternalServerError)
		return
	}

	out := make([]map[string]any, 0, len(linked))
	for _, i := range linked {
		// The provider subject is not published: it is the value the link is
		// keyed by.
		out = append(out, map[string]any{
			"provider":       i.Provider,
			"email":          i.Email,
			"connected_at":   i.CreatedAt,
			"can_disconnect": user.HasPassword() || len(linked) > 1,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"identities":   out,
		"has_password": user.HasPassword(),
		"available":    s.providerNames(),
	})
}

// handleUnlinkIdentity disconnects a provider.
//
// The rule that this must not be the last way in is enforced inside
// UnlinkIdentity's DELETE, not here. This handler only translates the refusal —
// which is the point: a second call site cannot forget a check it does not
// perform.
func (s *Server) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireSession(w, r)
	if !ok {
		return
	}
	name := r.PathValue("provider")

	err := s.db.UnlinkIdentity(r.Context(), user.ID, name)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{
			"message": providerLabel(name) + " is no longer connected.",
		})
	case errors.Is(err, store.ErrLastSignInMethod):
		jsonError(w, "that is the only way to sign in to this account — set a "+
			"password or connect another provider first", http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		jsonError(w, "that account is not connected", http.StatusNotFound)
	default:
		s.log.Error("unlink identity", "provider", name, "err", err)
		jsonError(w, "could not disconnect that account", http.StatusInternalServerError)
	}
}

// sessionUser resolves the session cookie, reporting whether there was one.
//
// Quiet: it writes nothing. requireSession is the version that answers the
// request, and handleOAuthStart wants the question without the answer.
func (s *Server) sessionUser(r *http.Request) (*store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	u, err := s.db.SessionUser(r.Context(), c.Value)
	if err != nil {
		return nil, false
	}
	return u, true
}

// requireSession resolves a signed-in account or refuses the request.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	u, ok := s.sessionUser(r)
	if !ok {
		jsonError(w, "sign in first", http.StatusUnauthorized)
		return nil, false
	}
	return u, true
}

// providerNames lists the configured providers, for a client deciding which
// buttons to draw.
func (s *Server) providerNames() []string {
	out := make([]string, 0, len(s.providers))
	for _, n := range []string{"github", "google"} {
		if _, ok := s.providers[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// configuredProviders returns the providers this deployment has credentials
// for. ConfigFromEnv has already refused a half-configured pair.
func configuredProviders(cfg Config, log *slog.Logger) map[string]oauth.Provider {
	out := map[string]oauth.Provider{}
	if cfg.GitHubClientID != "" && cfg.GitHubClientSecret != "" {
		out["github"] = oauth.NewGitHub(cfg.GitHubClientID, cfg.GitHubClientSecret)
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		out["google"] = oauth.NewGoogle(cfg.GoogleClientID, cfg.GoogleClientSecret)
	}

	names := make([]string, 0, len(out))
	for _, n := range []string{"github", "google"} {
		if _, ok := out[n]; ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		log.Info("no OAuth providers configured; sign-in is by email and password only")
	} else {
		log.Info("OAuth providers configured", "providers", strings.Join(names, ","))
	}
	return out
}

// providerLabel is the name a person recognises.
func providerLabel(name string) string {
	switch name {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	default:
		return name
	}
}
