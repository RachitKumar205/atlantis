package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	cloudmail "github.com/rachitkumar205/atlantis/internal/cloud/mail"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Server is Cloud's HTTP surface.
type Server struct {
	cfg     Config
	db      *store.Store
	mailer  cloudmail.Mailer
	breach  authn.BreachChecker
	iss     *issuer.Issuer
	log     *slog.Logger
	lim     *limiter
	mux     *http.ServeMux
	handler http.Handler

	// sleep is the latency floor's wait. A field so tests can record the
	// requested duration instead of spending it.
	sleep func(context.Context, time.Duration)

	bgCancel context.CancelFunc
}

// New wires the server. The store is owned by the caller and not closed here.
func New(cfg Config, db *store.Store, iss *issuer.Issuer, log *slog.Logger) *Server {
	s := &Server{
		cfg: cfg, db: db, iss: iss, log: log,
		lim:   newLimiter(),
		mux:   http.NewServeMux(),
		sleep: realSleep,
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
	s.handler = securityHeaders(s.mux)

	ctx, cancel := context.WithCancel(context.Background())
	s.bgCancel = cancel
	go s.sweepExpiredTokens(ctx)

	return s
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

	// Reached from an email, by a person, in a browser. Plain pages rather than
	// JSON for that reason. They are deliberately unstyled and framework-free:
	// the Cloud sign-in app replaces them, and a link in an email that already
	// works is worth more now than one that waits for a frontend.
	s.mux.HandleFunc("GET /verify", s.handleVerify)
	s.mux.HandleFunc("GET /reset", s.handleResetForm)
	s.mux.HandleFunc("POST /reset", s.handleResetSubmit)

	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// securityHeaders applies the same baseline the console uses.
//
// The pages here are tiny and carry no scripts, so the policy can be far
// stricter than a SPA's: nothing loads from anywhere.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// no-referrer matters more here than usual: these pages are reached
		// with a single-use token in the query string, and the default policy
		// would put that token in the Referer of anything the page loads.
		next.ServeHTTP(w, r)
	})
}

// sweepExpiredTokens removes tokens that can no longer be spent.
//
// Logs what it deleted including zero, because a sweep that silently deletes
// nothing forever is a shape this repository has shipped before — see the TTL
// sweeper in the CHANGELOG. A count in the log is the difference between
// "nothing to do" and "this has not worked for months".
func (s *Server) sweepExpiredTokens(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.db.DeleteExpiredEmailTokens(ctx)
			if err != nil {
				s.log.Error("sweep expired email tokens", "err", err)
				continue
			}
			s.log.Info("swept expired email tokens", "deleted", n)
		}
	}
}

// ── Small helpers ───────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v any) {
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
