package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	cloudmail "github.com/rachitkumar205/atlantis/internal/cloud/mail"
	"github.com/rachitkumar205/atlantis/internal/cloud/oauth"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/secrets"
	"github.com/rachitkumar205/atlantis/internal/spafs"
)

// readyProbeTimeout bounds the database check on /readyz.
//
// Shorter than a probe period, so a slow answer is reported as not-ready rather
// than arriving after the kubelet has already given up and counted a timeout —
// which looks the same from outside and says nothing about the database.
const readyProbeTimeout = 3 * time.Second

// Server is Cloud's HTTP surface.
type Server struct {
	cfg    Config
	db     *store.Store
	mailer cloudmail.Mailer
	breach authn.BreachChecker
	iss    *issuer.Issuer
	log    *slog.Logger
	lim    *limiter

	// keys seals each account's TOTP secret. See internal/secrets for what that
	// does and does not defend.
	keys secrets.Keyring

	// providers holds the OAuth providers that are configured, keyed by name.
	//
	// A provider with no client credentials is absent rather than present and
	// broken, which is what makes its routes answer 404. Populated before
	// routes() runs, and replaceable afterwards so a test can substitute a fake
	// — the same seam as mailer, breach and sleep.
	providers map[string]oauth.Provider

	// spaFS is the built sign-in application, or nil when the binary was
	// compiled without the embedspa tag. Nil is a supported state, not an
	// error: it is what every development and CI build looks like. See
	// cmd/cloud/spa_none.go.
	spaFS fs.FS

	mux     *http.ServeMux
	handler http.Handler

	// sleep is the latency floor's wait. A field so tests can record the
	// requested duration instead of spending it.
	sleep func(context.Context, time.Duration)

	bgCancel context.CancelFunc
}

// New wires the server. The store is owned by the caller and not closed here.
//
// Returns an error only for a keyset it cannot read: without one Cloud can
// neither enrol a second factor nor check one, so every sign-in would fail at
// the last step with a message about ciphertext rather than about
// configuration.
// spaFS is the built sign-in application. Nil is supported and means the SPA
// routes answer 404 naming the command that builds it — see cmd/cloud.
func New(cfg Config, db *store.Store, iss *issuer.Issuer, spaFS fs.FS, log *slog.Logger) (*Server, error) {
	keys, err := secrets.FromEnvKeyset(cfg.DataKeyset)
	if err != nil {
		return nil, fmt.Errorf("CLOUD_DATA_KEY: %w", err)
	}

	s := &Server{
		cfg: cfg, db: db, iss: iss, log: log,
		keys:      keys,
		spaFS:     spaFS,
		providers: configuredProviders(cfg, log),
		lim:       newLimiter(),
		mux:       http.NewServeMux(),
		sleep:     realSleep,
	}

	// No mail server means the logging mailer, which prints the link and warns
	// every time. See internal/cloud/mail for why the warning is not optional.
	if cfg.SMTPAddr != "" {
		s.mailer = &cloudmail.SMTP{
			Addr: cfg.SMTPAddr, From: cfg.SMTPFrom,
			Username: cfg.SMTPUser, Password: cfg.SMTPPassword,
			Timeout: cfg.SendTimeout,
		}
	} else {
		s.mailer = &cloudmail.Log{Logger: log}
	}

	if cfg.CheckBreaches {
		s.breach = authn.NewHIBP()
	} else {
		s.breach = authn.NoBreachCheck{}
	}

	s.routes()
	s.handler = s.securityHeaders(s.mux)

	ctx, cancel := context.WithCancel(context.Background())
	s.bgCancel = cancel
	go s.sweepExpired(ctx)

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Close stops background work. It does not close the store, which it does not
// own.
func (s *Server) Close() {
	if s.bgCancel != nil {
		s.bgCancel()
	}
}

func (s *Server) routes() {
	// The key set every console verifies against. Unchanged from what
	// `cloud serve` published before this package existed.
	s.mux.Handle("GET "+issuer.JWKSPath, s.iss.Handler())

	s.mux.HandleFunc("POST /api/auth/signup", s.handleSignup)
	s.mux.HandleFunc("POST /api/auth/reset/request", s.handleResetRequest)
	s.mux.HandleFunc("POST /api/auth/reset/complete", s.handleResetComplete)
	s.mux.HandleFunc("POST /api/auth/verify/resend", s.handleResendVerification)

	// Sign-in, in two legs. Nothing here issues a session except
	// handleVerifySecondFactor and the enrolment that completes a sign-in.
	//
	// JSON only. There was a script-free HTML enrolment page beside these, and
	// it went for the same reason WebAuthn is not here yet: nothing could reach
	// it. Sign-in returns JSON, so a person in a browser never arrives at the
	// page, and it confirmed a factor without completing the sign-in the way
	// these routes do. The verification and reset pages below are different —
	// each is the target of a link in an email somebody receives. Enrolment
	// gets its page from the Cloud sign-in app, along with the QR code.
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/2fa/verify", s.handleVerifySecondFactor)
	s.mux.HandleFunc("POST /api/auth/2fa/enrol/begin", s.handleEnrolBegin)
	s.mux.HandleFunc("POST /api/auth/2fa/enrol/finish", s.handleEnrolFinish)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// What the sign-in screen needs before anybody has signed in.
	//
	// Unauthenticated by necessity: the page has to decide whether to draw a
	// "Continue with GitHub" button before there is a session to ask about.
	// providerNames() was otherwise reachable only through
	// handleListIdentities, which starts with requireSession.
	//
	// It discloses which providers this deployment configured, which is already
	// observable — /auth/github either redirects to GitHub or it does not.
	s.mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)

	// Whether a half-finished sign-in is in progress, and what it needs next.
	//
	// The pending cookie is HttpOnly, so a reloaded page cannot read it and has
	// no other way to ask. Without this the app shows a fresh sign-in form to
	// somebody who is mid-enrolment; on the password path they can retype a
	// password, but on the OAuth path there is nothing to retype and the whole
	// provider round trip has to be done again for no visible reason.
	s.mux.HandleFunc("GET /api/auth/pending", s.handlePendingState)

	// Signing in through a provider. Registered per provider and only when it
	// is configured, so an unconfigured one is absent rather than present and
	// failing — see configuredProviders.
	for name := range s.providers {
		s.mux.HandleFunc("GET /auth/"+name, s.handleOAuthStart(name))
		s.mux.HandleFunc("GET /auth/"+name+"/callback", s.handleOAuthCallback(name))
	}

	// Managing connected accounts, registered UNCONDITIONALLY.
	//
	// Not behind the same check as the routes above, and the difference matters:
	// removing a provider's credentials must not strand the people who already
	// linked it with no way to see the connection or remove it. The link exists
	// in the database whether or not Cloud can still start a sign-in with it.
	s.mux.HandleFunc("GET /api/account/identities", s.handleListIdentities)
	s.mux.HandleFunc("POST /api/account/identities/{provider}/unlink", s.handleUnlinkIdentity)

	// The signed-in account and its organisations.
	//
	// The first routes Cloud has ever served that answer for an established
	// session rather than a pre-session state, which is why the sign-in
	// application has never had a signed-in mode: there was nothing to ask.
	s.mux.HandleFunc("GET /api/account/me", s.handleMe)
	s.mux.HandleFunc("GET /api/orgs/{org}", s.handleGetOrg)

	// The route that ends `cloud org create`.
	//
	// The first state-changing thing in Cloud a script could reach, which is
	// why sameOrigin exists and why it is required here rather than on the two
	// form posts — see its comment.
	s.mux.HandleFunc("POST /api/orgs", s.handleCreateOrg)

	// Handing a signed-in user to an organisation's console.
	//
	// Reached by a browser rather than a script — from a link, or from the
	// popup the console opens for step-up — so these answer with pages and
	// redirects, not JSON.
	s.mux.HandleFunc("GET /authorize", s.handleAuthorize)
	s.mux.HandleFunc("POST /authorize/reauth", s.handleReauth)

	// Reached from an email, by a person, in a browser. Plain pages rather than
	// JSON for that reason. They are deliberately unstyled and framework-free:
	// the Cloud sign-in app replaces them, and a link in an email that already
	// works is worth more now than one that waits for a frontend.
	s.mux.HandleFunc("GET /verify", s.handleVerify)
	s.mux.HandleFunc("GET /reset", s.handleResetForm)
	s.mux.HandleFunc("POST /reset", s.handleResetSubmit)

	// Liveness. Deliberately answers without touching anything: a liveness probe
	// that failed when the database hiccupped would restart a healthy process
	// and turn a recoverable outage into a crash loop.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness, which is a different question and used to have no answer.
	//
	// Cloud's Deployment had to probe either /healthz — a bare 200 that reports
	// success while Postgres is unreachable — or the JWKS route, which proves
	// only that the signing key loaded. Under both, this process reports itself
	// ready to serve sign-ins it cannot complete: every account lookup, every
	// membership check and every session write is a database call.
	//
	// So the load balancer sends it traffic and each request fails
	// individually, which is the outage this route exists to convert into "this
	// replica is not ready".
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		// Bounded, so a wedged database cannot hold a probe open for the whole
		// request and make readiness itself the thing that hangs.
		ctx, cancel := context.WithTimeout(r.Context(), readyProbeTimeout)
		defer cancel()
		if err := s.db.Pool().Ping(ctx); err != nil {
			http.Error(w, "cloud db: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// ── The sign-in application ─────────────────────────────────────────────
	//
	// Registered last, and last on purpose: `GET /` matches every GET path, so
	// everything above has to be more specific to survive. It is — every
	// pattern in this function is method-qualified and names a literal path, so
	// net/http's conflict rule never fires and each one still wins.
	//
	// `GET /` rather than a method-less `/`, unlike the console
	// (internal/console/server.go:450). A POST to a page route is a mistake,
	// and 405 says so; the console answers it with index.html.
	s.mux.Handle("GET /", s.withSPAPolicy(http.HandlerFunc(s.handleSPA)))

	// An unmatched API path must not be answered by the app.
	//
	// Without these, `GET /api/typo` falls to the catch-all and returns 200
	// with index.html — which reaches a client as a JSON parse error naming
	// something that has nothing to do with the mistake.
	//
	// Both methods, because registering only GET is worse than registering
	// neither: `POST /api/typo` would then match `GET /api/`'s path but not its
	// method, and net/http answers that with 405 and a text/plain body.
	// PUT/DELETE/PATCH still do — Cloud has no such routes, so nothing can
	// reach it today, and this is the shape to extend when one appears.
	s.mux.HandleFunc("GET /api/", notFoundJSON)
	s.mux.HandleFunc("POST /api/", notFoundJSON)

	// Same trap, one level up. OAuth routes are registered only for providers
	// this deployment has credentials for, so `/auth/google` on a deployment
	// with no Google credentials used to be a clean 404 — and behind the
	// catch-all would become 200 index.html. A literal segment beats a
	// wildcard, so a configured `GET /auth/github` still wins over these.
	s.mux.HandleFunc("GET /auth/{provider}", notFoundJSON)
	s.mux.HandleFunc("GET /auth/{provider}/callback", notFoundJSON)
}

// notFoundJSON answers a path that looks like an API route and is not one.
func notFoundJSON(w http.ResponseWriter, _ *http.Request) {
	jsonError(w, "no such route", http.StatusNotFound)
}

// handleSPA serves the sign-in application.
//
// A method reading s.spaFS per request, rather than a handler built once in
// routes() around the field's value. The difference is not stylistic: building
// it once captures whatever spaFS held at construction, which makes the struct
// field a lie afterwards and silently ignores anything set later. A test
// swapping in a filesystem is the case that found it, and a test that cannot
// see its own setup take effect is the failure mode.
//
// internal/console does the same for the same reason.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	spafs.Handler(s.spaFS, "sign-in app not built yet — run: make build-cloud-spa").
		ServeHTTP(w, r)
}

// withSPAPolicy widens the CSP for the one handler that needs it.
//
// This is the whole of the opt-in. securityHeaders has already set strictCSP by
// the time this runs, and nothing has been flushed, so overwriting the header
// here is what makes the sign-in application loadable — and leaves every other
// route on `default-src 'none'` without having to remember to.
func (s *Server) withSPAPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", spaCSP)
		next.ServeHTTP(w, r)
	})
}

// strictCSP is the policy every route gets unless it says otherwise.
//
// `default-src 'none'` — nothing loads from anywhere. It fits every route Cloud
// had before the sign-in application: JSON, and three script-free pages reached
// with a single-use token in the query string.
const strictCSP = "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// spaCSP is the policy for the sign-in application, and only for it.
//
// Wider than strictCSP because a React page has to load its own bundle, and
// narrower than the console's because this is the origin holding every
// password, every TOTP secret and the assertion signing key:
//
//   - `script-src 'self'` with no 'unsafe-inline' — the directive that actually
//     stops injected code running.
//   - `style-src 'self'` with no 'unsafe-inline', which the console does allow.
//     The cost is a rule: no inline style attributes anywhere in web/cloud, so
//     no `style={{…}}` and no `style={{'--x': v}}` either — a style attribute is
//     governed by style-src-attr, which falls back to here. Dynamic values go
//     through a class or ref + el.style.setProperty(), which is CSSOM and not
//     governed by CSP.
//   - No `data:` in img-src, so Vite must not inline small assets
//     (assetsInlineLimit: 0). The QR code is inline SVG rectangles, which needs
//     no allowance at all.
//   - No external host anywhere. Geist is self-hosted rather than fetched from
//     Google, because a page where passwords are typed should make no
//     third-party request.
const spaCSP = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// securityHeaders applies the baseline to every response.
//
// Fail-closed by construction: the strict policy is set HERE, before the mux
// runs, and a handler that needs something wider overwrites it. Headers are not
// flushed until WriteHeader, so the overwrite wins — and a route added later
// that does not think about CSP inherits `default-src 'none'` rather than
// whatever the loosest route needed.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", strictCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// no-referrer matters more here than usual: these pages are reached
		// with a single-use token in the query string, and the default policy
		// would put that token in the Referer of anything the page loads.
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		// Only over HTTPS. HSTS on plain HTTP is a foot-gun, and the console
		// gates it on the same value for the same reason.
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// sweepExpired removes tokens, sessions and half-finished logins that can no
// longer be used.
//
// Logs what it deleted including zero, because a sweep that silently deletes
// nothing forever is a shape this repository has shipped before — see the TTL
// sweeper in the CHANGELOG. A count in the log is the difference between
// "nothing to do" and "this has not worked for months".
func (s *Server) sweepExpired(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tokens, err := s.db.DeleteExpiredEmailTokens(ctx)
			if err != nil {
				s.log.Error("sweep expired email tokens", "err", err)
				continue
			}
			sessions, pending, err := s.db.DeleteExpiredSessions(ctx)
			if err != nil {
				s.log.Error("sweep expired sessions", "err", err)
				continue
			}
			s.log.Info("swept expired rows",
				"email_tokens", tokens, "sessions", sessions, "pending_logins", pending)
		}
	}
}

// ── Small helpers ───────────────────────────────────────────────────────────

// writeJSON answers with a JSON body that is never stored.
//
// no-store on every response rather than on the ones that need it, for the same
// reason page() does it: the list of routes carrying something private is not
// stable, and the failure is silent. Two of them already do —
// /api/auth/2fa/enrol/begin returns the raw TOTP secret, and
// /api/auth/2fa/enrol/finish returns the backup codes in plaintext — and both
// were cacheable until this line existed.
//
// Nothing here is worth caching in the first place: every route is a state
// change or an answer scoped to one session.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decode reads a bounded JSON body.
//
// Bounded because these routes are unauthenticated: without a limit, one
// request can make this process allocate until it dies.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		jsonError(w, "malformed request", http.StatusBadRequest)
		return false
	}
	return true
}

// validEmail reports whether an address is one this product will send to.
//
// net/mail.ParseAddress accepts a great deal that is technically legal and
// practically not an address anyone has — display names, angle brackets, source
// routes. The extra checks narrow it to something that will survive being put
// in a To: header and typed back by a human.
func validEmail(addr string) bool {
	if len(addr) > 254 || strings.ContainsAny(addr, "\r\n ") {
		return false
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil || parsed.Name != "" || parsed.Address != addr {
		return false
	}
	at := strings.LastIndex(addr, "@")
	return at > 0 && strings.Contains(addr[at+1:], ".")
}

// link builds an absolute URL into Cloud from the configured public base.
func (s *Server) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.cfg.PublicURL, path, token)
}
