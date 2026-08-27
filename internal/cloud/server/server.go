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

	// Which transport sends the two messages that gate account recovery.
	//
	// Config.validateMail has already refused everything ambiguous — no
	// transport, both transports, a transport with no sender — so this is a
	// choice between configurations already known to be complete rather than a
	// fallback chain. The logging mailer is reached only by asking for it.
	switch {
	case cfg.MailDev:
		s.mailer = &cloudmail.Log{Logger: log}
	case cfg.ResendAPIKey != "":
		s.mailer = &cloudmail.Resend{
			APIKey: cfg.ResendAPIKey, From: cfg.MailFrom,
			Timeout: cfg.SendTimeout, Logger: log,
		}
	default:
		s.mailer = &cloudmail.SMTP{
			Addr: cfg.SMTPAddr, From: cfg.MailFrom,
			Username: cfg.SMTPUser, Password: cfg.SMTPPassword,
			Timeout: cfg.SendTimeout,
		}
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
	// The key set every console verifies against.
	s.mux.Handle("GET "+issuer.JWKSPath, s.iss.Handler())

	s.mux.HandleFunc("POST /api/auth/signup", s.handleSignup)
	s.mux.HandleFunc("POST /api/auth/reset/request", s.handleResetRequest)
	s.mux.HandleFunc("POST /api/auth/reset/complete", s.handleResetComplete)
	s.mux.HandleFunc("POST /api/auth/verify/resend", s.handleResendVerification)

	// Sign-in, in two legs. Only handleVerifySecondFactor and the enrolment
	// that completes a sign-in issue a session.
	//
	// JSON only, unlike the verification and reset pages below, which are the
	// targets of links in mail. The sign-in app renders enrolment and its QR
	// code.
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/auth/2fa/verify", s.handleVerifySecondFactor)
	s.mux.HandleFunc("POST /api/auth/2fa/enrol/begin", s.handleEnrolBegin)
	s.mux.HandleFunc("POST /api/auth/2fa/enrol/finish", s.handleEnrolFinish)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// What the sign-in screen needs before a session exists, so it is
	// unauthenticated: the page decides whether to draw a provider button
	// before there is a session to ask about.
	//
	// It discloses which providers are configured, which /auth/github already
	// does by either redirecting or answering 404.
	s.mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)

	// Whether a half-finished sign-in is in progress, and what it needs next.
	//
	// The pending cookie is HttpOnly, so a reloaded page cannot read it. Without
	// this route the app draws a fresh sign-in form mid-enrolment, and on the
	// OAuth path that means repeating the whole provider round trip.
	s.mux.HandleFunc("GET /api/auth/pending", s.handlePendingState)

	// Signing in through a provider, registered only for configured ones.
	for name := range s.providers {
		s.mux.HandleFunc("GET /auth/"+name, s.handleOAuthStart(name))
		s.mux.HandleFunc("GET /auth/"+name+"/callback", s.handleOAuthCallback(name))
	}

	// Managing connected accounts, registered unconditionally. A link survives
	// in the database after its provider's credentials are removed, and has to
	// remain visible and removable.
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

	// Deletion and restore are not symmetrical. Delete requires the
	// organisation's own name in the body; restore requires nothing beyond
	// membership, so undoing a mistake is never harder than making it.
	//
	// Neither destroys anything. Both write a row; the provisioner is the only
	// process that touches the cluster.
	s.mux.HandleFunc("POST /api/orgs/{org}/delete", s.handleDeleteOrg)
	s.mux.HandleFunc("POST /api/orgs/{org}/restore", s.handleRestoreOrg)

	// Handing a signed-in user to an organisation's console.
	//
	// Reached by a browser rather than a script — from a link, or from the
	// popup the console opens for step-up — so these answer with pages and
	// redirects, not JSON.
	s.mux.HandleFunc("GET /authorize", s.handleAuthorize)
	s.mux.HandleFunc("POST /authorize/reauth", s.handleReauth)

	// Reached from an email, by a person, in a browser, so these answer with
	// plain pages rather than JSON. Unstyled and framework-free; the Cloud
	// sign-in app replaces them.
	s.mux.HandleFunc("GET /verify", s.handleVerify)
	s.mux.HandleFunc("GET /reset", s.handleResetForm)

	// Where a console sends the browser to end its Cloud session. GET because a
	// redirect is what carries it here.
	s.mux.HandleFunc("GET /logout", s.handleEndSession)
	s.mux.HandleFunc("POST /reset", s.handleResetSubmit)

	// Liveness, answering without touching anything. A liveness probe that
	// failed on a database hiccup would restart a healthy process and turn a
	// recoverable outage into a crash loop.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness is a different question from liveness. Every account lookup,
	// membership check and session write is a database call, so a replica that
	// answers /healthz while Postgres is unreachable reports itself ready to
	// serve sign-ins it cannot complete, and the load balancer sends it traffic
	// that fails one request at a time.
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

	// The sign-in application, registered last because `GET /` matches every GET
	// path. Everything above it is method-qualified and names a literal path, so
	// net/http's conflict rule never fires and each one still wins.
	//
	// `GET /` rather than a method-less `/`, unlike the console
	// (internal/console/server.go:450): a POST to a page route is a mistake, and
	// 405 says so where index.html would not.
	s.mux.Handle("GET /", s.withSPAPolicy(http.HandlerFunc(s.handleSPA)))

	// An unmatched API path must not be answered by the app.
	//
	// Without these, `GET /api/typo` falls to the catch-all and returns 200
	// with index.html, which reaches a client as a JSON parse error.
	//
	// Both methods. With only GET registered, `POST /api/typo` matches
	// `GET /api/`'s path but not its method, and net/http answers 405 with a
	// text/plain body. PUT, DELETE and PATCH still do; Cloud has no such
	// routes, and this is the shape to extend when one appears.
	s.mux.HandleFunc("GET /api/", notFoundJSON)
	s.mux.HandleFunc("POST /api/", notFoundJSON)

	// Same trap, one level up. OAuth routes are registered only for providers
	// this deployment has credentials for, so behind the catch-all
	// `/auth/google` with no Google credentials would answer 200 index.html
	// instead of 404. A literal segment beats a wildcard, so a configured
	// `GET /auth/github` still wins over these.
	s.mux.HandleFunc("GET /auth/{provider}", notFoundJSON)
	s.mux.HandleFunc("GET /auth/{provider}/callback", notFoundJSON)
}

// notFoundJSON answers a path that looks like an API route and is not one.
func notFoundJSON(w http.ResponseWriter, _ *http.Request) {
	jsonError(w, "no such route", http.StatusNotFound)
}

// handleSPA serves the sign-in application.
//
// Reads s.spaFS per request. A handler built once in routes() captures whatever
// the field held at construction and ignores anything assigned later, including
// a test's filesystem. internal/console does the same.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	spafs.Handler(s.spaFS, "sign-in app not built yet — run: make build-cloud-spa").
		ServeHTTP(w, r)
}

// withSPAPolicy widens the CSP for the one handler that needs it.
//
// securityHeaders has already set strictCSP by the time this runs and nothing
// has been flushed, so overwriting the header here is the whole opt-in. Every
// other route keeps `default-src 'none'`.
func (s *Server) withSPAPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", spaCSP)
		next.ServeHTTP(w, r)
	})
}

// strictCSP is the policy every route gets unless it says otherwise.
//
// `default-src 'none'`: nothing loads from anywhere. It fits the JSON routes
// and the three script-free pages reached with a token in the query string.
const strictCSP = "default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// spaCSP is the Content-Security-Policy for the sign-in application. Wider than
// strictCSP, which the plain pages use, because a React page loads its own
// bundle.
//
//   - `script-src 'self'`, no 'unsafe-inline'.
//   - `style-src 'self'`, no 'unsafe-inline', which the console does allow.
//     This forbids inline style attributes anywhere in web/cloud: a style
//     attribute is governed by style-src-attr, which falls back to style-src.
//     Dynamic values go through a class, or ref + el.style.setProperty(), which
//     is CSSOM and not governed by CSP.
//   - No `data:` in img-src, so Vite must not inline small assets
//     (assetsInlineLimit: 0). The QR code is inline SVG rectangles.
//   - No external host. Geist is self-hosted rather than fetched from Google.
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
// net/mail.ParseAddress accepts display names, angle brackets and source
// routes. The extra checks narrow it to an address that survives a To: header.
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
