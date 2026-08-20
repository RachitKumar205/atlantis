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
//
// A third cookie beside the session and the pending login, for the same reason
// those two are separate from each other: it holds a different thing at a
// different stage, and one that can be mistaken for another is one that will be.
const oauthCookie = "atl_cloud_oauth"

// oauthTTL bounds how long a started sign-in can be finished.
//
// Long enough to read a consent screen and think about it, short enough that a
// cookie left on a shared machine is not a way back in. Shorter than the
// pending login it produces, because this half involves no decision by Cloud.
const oauthTTL = 10 * time.Minute

// Intents. Whether the callback is signing somebody in or attaching a provider
// to an account that is already signed in.
const (
	intentSignIn = "signin"
	intentLink   = "link"
)

// oauthState is what the cookie holds.
//
// # Why a cookie and not a table
//
// The value is per-browser by nature, and the browser is the thing holding it.
// A table would cost a row per abandoned sign-in, a sweeper, and an exemption
// in the boot-time policy guard arguing that nobody is identified when a
// callback lands. None of that buys a property this does not have.
//
// # What it does and does not say
//
// It says: this callback belongs to a request that started here, for this
// provider, doing this. It does NOT say who — for a link, the acting account
// comes from the session cookie at callback time. Putting a user id in here
// would make a forged or replayed blob a way to attach a provider account to
// somebody else's Cloud account.
type oauthState struct {
	Provider string
	Intent   string
	State    string
	Verifier string

	// Expires is checked here rather than left to the cookie's MaxAge.
	//
	// MaxAge is a request the browser honours; it is not a rule. Anything that
	// keeps a copy of the value can present it afterwards, so the deadline has
	// to be inside the thing being presented.
	Expires time.Time
}

// encode packs the state into a cookie value.
//
// Five fields joined by a character that appears in none of them: the two
// tokens are base64url, the provider and intent are constants from this file,
// and the deadline is decimal. Not JSON, which would need escaping rules for
// values that cannot contain the separator anyway.
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

// redirectURI is where the provider sends the browser back.
//
// Built from CLOUD_PUBLIC_URL and never from anything in the request. A
// redirect_uri taken from a parameter is the open-redirect version of this
// flow: the provider would faithfully deliver the authorization code to
// whatever host was asked for.
func (s *Server) redirectURI(provider string) string {
	return fmt.Sprintf("%s/auth/%s/callback", s.cfg.PublicURL, provider)
}

// handleOAuthStart sends the browser to a provider.
//
// A factory closed over the provider's name rather than a handler that reads it
// from the path. The route decides which provider this is, so nothing in the
// request can influence which endpoint the authorization code is later
// exchanged at — and an unconfigured provider has no route at all, which is
// what makes its URL a 404 rather than an error page for a feature nobody
// turned on.
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

	// A session means this is somebody attaching a second way to sign in, not
	// somebody signing in. Read here rather than at the callback so the intent
	// is fixed before the round trip — a session that expires mid-consent
	// should not silently turn a link into a new account.
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
// # The one thing this must not do
//
// Issue a session. Every path through here ends in a pending login, which the
// second factor turns into a session through the routes C3 already has. A
// provider vouching for somebody is one factor; treating it as two is the
// bypass this whole design exists to prevent.
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
	// Cleared before anything else can fail. One authorization code, one
	// attempt: a cookie surviving a failed callback is a cookie that can be
	// replayed against a second one.
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
	// Constant time. The comparison is against a value an attacker supplies and
	// can vary, which is the shape a timing oracle needs.
	if subtle.ConstantTimeCompare([]byte(st.State), []byte(r.URL.Query().Get("state"))) != 1 {
		page(w, http.StatusBadRequest, "This sign-in could not be completed. Please try again.")
		return
	}

	// The provider refused, or the user declined consent. Not an error on this
	// side, and the provider's own description is not echoed back: it is text
	// from a third party rendered into a page.
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
		// Known link. The common path, every sign-in after the first.
	case errors.Is(err, store.ErrNotFound):
		var ok bool
		user, ok = s.adoptOrCreate(w, r, provider, id)
		if !ok {
			// Answered already: created nothing, refused, or failed. Which one
			// is not this function's business — every branch in there writes
			// its own response, and a second one here would be a second body.
			return
		}
	default:
		s.log.Error("look up identity", "provider", provider, "err", err)
		page(w, http.StatusInternalServerError, "Could not sign in. Please try again.")
		return
	}

	s.startSecondLeg(w, r, user, provider)
}

// adoptOrCreate handles a provider account nobody has linked yet.
//
// Reports false when it has already answered the request, which covers every
// outcome except a user the caller should carry on with — created, refused, or
// failed. It returns no error for the same reason: each of those branches has
// its own body to write, and handing one back would mean the caller wrote a
// second.
func (s *Server) adoptOrCreate(w http.ResponseWriter, r *http.Request, provider string, id *oauth.Identity) (*store.User, bool) {
	ctx := r.Context()

	existing, err := s.db.UserByEmail(ctx, id.Email)
	if errors.Is(err, store.ErrNotFound) {
		// Nobody here yet. One transaction, so there is no state where the
		// account exists without the link that reaches it.
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

	// # The auto-link rule, and why it has a condition
	//
	// The provider says this person controls an address that already has a
	// Cloud account. Attaching the two is what makes "Sign in with GitHub"
	// work for somebody who signed up with a password.
	//
	// It is safe only while a second factor stands behind it. Cloud is trusting
	// a third party's word about an address, and if that word is enough to
	// reach an account with nothing else on it, then registering the right
	// address at a provider is account takeover: the attacker would arrive at
	// enrolment and set their own factor.
	//
	// So the account must already have one. Then the provider's word gets the
	// attacker as far as a challenge they cannot answer, which is exactly where
	// a stolen password gets them.
	//
	// A factorless account is refused and NOTHING is written — no identity row,
	// no pending login. The person is told to use the password they already
	// have, which puts them through the same enrolment they were going to need.
	// Unverified accounts get their own answer, and it has to come first.
	//
	// The refusal below tells people to sign in with their password — which
	// handleLogin refuses for an unverified address, so an account in that
	// state would be told to do the one thing that cannot work. Point at the
	// verification link instead.
	//
	// No mail is sent from here. A callback that mailed an address chosen by
	// whoever holds a provider account is a way to send mail to somebody at
	// will, so the resend route stays the only thing that sends.
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
	// Refused HERE, before anything is written and before any pending login can
	// be minted. That ordering is the safety property, not a nicety: the branch
	// below hands out a pending login with mayEnrol computed from `enrolled`,
	// and if a factorless account ever reached it, the person holding the
	// provider account would be handed an ENROLLING pending login for somebody
	// else's account. That is takeover in two clicks. The refusal is what makes
	// the auto-link above acceptable, so it cannot be relaxed on its own.
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
// Deliberately the same two calls handleLogin makes after a correct password,
// in the same order, so there is one definition of what a first factor earns.
func (s *Server) startSecondLeg(w http.ResponseWriter, r *http.Request, user *store.User, provider string) {
	// Any session already in this browser belongs to whoever used it last, and
	// it is not the account being signed in to now. Left in place it outranks
	// the pending login at requireEnrolable, which prefers a session — so
	// enrolment would set a factor on the wrong account and hand back a session
	// for it. Signing in as somebody ends the previous somebody.
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
	// this file carries `provider=<id>`, and the page renders the label — one
	// spelling in the contract, and the app already has to know the ids to draw
	// its buttons.
	s.handOff(w, r, next, provider)
}

// handOff returns the browser to the sign-in application.
//
// # Relative, and no longer configurable
//
// This used to redirect to CLOUD_SIGNIN_APP_URL, and to answer with a
// text/plain page naming an HTTP route when that was unset. Both are gone:
// Cloud serves the application itself now, so the setting could only ever
// duplicate CLOUD_PUBLIC_URL — two values an operator keeps in step by hand,
// with a stale one stranding sign-ins on another origin after the pending
// cookie was already set on this one. That is the failure the console_url
// migration removed by fusing an audience and a redirect into one column, and
// it is not worth reintroducing.
//
// A relative Location resolves against this origin, which is exactly where the
// application is. `next` is a fixed literal here and in every caller, so there
// is nothing to escape and nothing to open-redirect through.
// provider is carried so the page can say which one signed you in.
func (s *Server) handOff(w http.ResponseWriter, r *http.Request, next, provider string) {
	http.Redirect(w, r, "/signin?next="+next+"&provider="+url.QueryEscape(provider),
		http.StatusSeeOther)
}

// refuseClaimed sends a sign-in back to the application with the reason.
//
// It used to answer with a text/plain 409 — accurate, and a dead end: no link
// back, and nothing the application could read to explain what happened. The
// link path has its own version of this (backToAccount with "claimed"), because
// somebody connecting a provider from account settings should land back there
// rather than on the sign-in screen.
func (s *Server) refuseClaimed(w http.ResponseWriter, r *http.Request, provider string) {
	http.Redirect(w, r,
		"/signin?error=claimed&provider="+url.QueryEscape(provider),
		http.StatusSeeOther)
}

// ── Linking, for somebody already signed in ─────────────────────────────────

// finishLink attaches a provider account to the signed-in account.
//
// The user comes from the session, not from the state cookie. That is the whole
// reason the state carries no identity: this is the branch where a forged blob
// would otherwise choose whose account gets a new way in.
func (s *Server) finishLink(w http.ResponseWriter, r *http.Request, provider string, id *oauth.Identity) {
	user, ok := s.sessionUser(r)
	if !ok {
		// The session went away during the round trip. Not an error worth a
		// stack trace — say what happened and let them start again.
		page(w, http.StatusUnauthorized,
			"You were signed out while connecting "+providerLabel(provider)+
				". Sign in and try again.")
		return
	}

	if err := s.db.LinkIdentity(r.Context(), user.ID, provider, id.Subject, id.Email); err != nil {
		if errors.Is(err, store.ErrIdentityClaimed) {
			// The account screen, not the sign-in one: this person is signed in
			// and was connecting a provider, so the refusal belongs where they
			// started.
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
// Every one of these used to be a text/plain page — "GitHub is now connected to
// your account." and nothing else: no link, no way back, and no way for the
// application to know what happened. Connecting a provider from account
// settings was a one-way trip out of the app.
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
		// The provider subject is not published. It is an opaque id nobody can
		// act on, and it is the value the link is keyed by.
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

// ── Shared helpers ──────────────────────────────────────────────────────────

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

// configuredProviders builds the providers this deployment has credentials for.
//
// # Why absent rather than disabled
//
// A provider in this map has routes; one that is not has none, and the URL
// answers 404. The alternative — always registering the routes and failing at
// the redirect — produces an error page for a feature nobody turned on, which
// reads as a broken deployment rather than an unconfigured one.
//
// ConfigFromEnv has already refused a half-configured pair, so reaching here
// with an id and no secret is not possible through the normal path.
//
// The log line names what is on. An operator who set the variables and sees
// nothing here has learned something at boot rather than from a user.
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
