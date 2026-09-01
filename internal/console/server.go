package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/rachitkumar205/atlantis/clients/go/adminjson"
	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/console/cloudauth"
	"github.com/rachitkumar205/atlantis/internal/migrate"
	"github.com/rachitkumar205/atlantis/internal/secrets"
	"github.com/rachitkumar205/atlantis/internal/spafs"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
	"github.com/rachitkumar205/atlantis/migrations"
)

const (
	sessionCookieName = "atl_console_session"
)

// Server is the atlantis console BFF.
type Server struct {
	cfg Config

	// orgs holds one admin channel per organisation, each dialled with that
	// organisation's own certificate against its own CA.
	//
	// This replaced a single `atl *adminClient`. One shared channel could only
	// ever reach one organisation's atlantis, and pointing it at the wrong one
	// returned the wrong organisation's schema, plans and jobs with no error
	// anywhere — the connection was healthy, it was simply the wrong stack.
	orgs *orgClients

	db       *store
	mux      *http.ServeMux
	handler  http.Handler // mux wrapped with security headers
	log      *slog.Logger
	spaFS    fs.FS
	loginLim *loginLimiter

	// cloud verifies the assertions this console accepts as identity. It is
	// the console's only source of one — there are no local accounts.
	cloud *cloudauth.Verifier

	// sandboxes owns the in-process sandbox runtime + per-user meta.
	// See internal/console/sandbox.go for the layer's design.
	sandboxes *sandboxLayer

	// signer is the console's mTLS client to the certificate signer, or nil
	// when enrolment is not configured.
	//
	// Built once at New() so a bad certificate pair is a startup failure rather
	// than something the first person to enrol a machine discovers.
	signer *http.Client

	// enrollSrv is the second listener, or nil. It carries the enrolment
	// routes and nothing else — see enroll.go for why it cannot share the mux.
	enrollSrv *http.Server

	// Cancelled by Close() to stop background workers (audit retention,
	// sandbox TTL janitor).
	bgCtx    context.Context
	bgCancel context.CancelFunc
}

// New wires the console server. spaFS is the embedded SPA filesystem
// (the built dist/ directory from web/console). If nil, the SPA fallback
// returns 404 — useful during development when the SPA runs separately.
func New(cfg Config, spaFS fs.FS, log *slog.Logger) (*Server, error) {
	// The keyring first: without it the console can neither read nor write an
	// organisation's credentials, so every page would fail at the first
	// request. Better to refuse to start.
	keys, err := secrets.FromEnvKeyset(cfg.DataKeyset)
	if err != nil {
		return nil, fmt.Errorf("CONSOLE_DATA_KEY: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// No dial here. Channels are per organisation and built on first use from a
	// registry row, so there is nothing to connect to at boot and nothing for
	// the failure paths below to unwind.
	db, err := newStore(ctx, cfg.PGURL, log, keys)
	if err != nil {
		return nil, fmt.Errorf("open console db: %w", err)
	}
	// Before anything touches the schema: refuse a role that reads through
	// row-level security. See consoleRoleError for why this is fatal and why
	// the decision lives in its own function.
	privs, derr := pg.DetectRolePrivileges(ctx, pg.PgxRoleQuerier{Q: db.pool})
	if err := consoleRoleError(privs, derr); err != nil {
		db.close()
		return nil, err
	}
	// The console's schema is versioned and travels inside this binary. See
	// the migrations package for why it is embedded rather than read from a
	// path, and internal/migrate for the search_path pin that keeps a second
	// boot from replaying the whole history.
	if err := migrate.RunFS(cfg.PGURL, migrations.Console, "console",
		migrate.ConsoleHistoryTable, log); err != nil {
		db.close()
		return nil, fmt.Errorf("console db migrate: %w", err)
	}
	// Partitions are per-month and cannot live in a static migration; the
	// migration creates the partitioned parent, this creates the children.
	// It also isolates each child, which is not inherited — see the function.
	if err := db.ensureAuditPartitions(ctx); err != nil {
		db.close()
		return nil, fmt.Errorf("console audit partitions: %w", err)
	}
	// Then ask the catalogue whether the organisation boundary is really there.
	//
	// After the migrations and the partitions, because it checks the state they
	// produce. Fatal, because a console that cannot separate organisations
	// looks completely healthy from the outside while serving each of them the
	// others' data.
	if err := verifyConsolePolicies(ctx, db.pool); err != nil {
		db.close()
		return nil, err
	}

	// The verifier is built before the server exists, so a console that
	// cannot tell who is signing in never starts. There is no other source of
	// identity to fall back to.
	cloud, err := cloudauth.New(cloudauth.Config{
		Issuer:   cfg.CloudIssuer,
		Audience: cfg.CloudAudience,
		JWKSURL:  cfg.CloudJWKSURL,
	})
	if err != nil {
		db.close()
		return nil, err
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, db: db, log: log, spaFS: spaFS,
		loginLim:  newLoginLimiter(),
		orgs:      newOrgClients(db),
		cloud:     cloud,
		sandboxes: newSandboxLayer(cfg.SandboxPerUserLimit, cfg.SandboxTTL),
		bgCtx:     bgCtx, bgCancel: bgCancel,
	}
	// The fallback signer and the enrolment listener, built independently.
	//
	// Independently, because a hosted console has no process-wide signer
	// address: every signer belongs to an organisation and signerFor picks the
	// right one per request. One branch covering both would make a listener
	// impossible without that address. See Config.EnrollmentEnabled.
	//
	// Both at startup, both fatal. validateEnrollment has already refused a
	// half-configured group, so reaching here with a broken certificate path
	// means a file that is missing or unreadable — exactly the failure that must
	// not wait until somebody needs a certificate.
	if cfg.FallbackSignerConfigured() {
		signer, serr := newSignerClient(cfg)
		if serr != nil {
			db.close()
			return nil, serr
		}
		s.signer = signer
	}
	if cfg.EnrollmentEnabled() {
		if serr := s.buildEnrollListener(); serr != nil {
			db.close()
			return nil, serr
		}
	}

	// Clean up embedded-pg tempdirs left over from a prior crashed
	// process before any new sandboxes are booted; idempotent and
	// best-effort.
	sweepEmbeddedTempdirs(func(msg string, args ...any) {
		log.Warn(fmt.Sprintf(msg, args...))
	})
	s.buildMux()
	s.handler = s.withSecurityHeaders(s.mux)
	go s.auditRetentionLoop()
	go s.sandboxes.runJanitor(bgCtx, 60*time.Second)
	return s, nil
}

func (s *Server) Close() {
	if s.bgCancel != nil {
		s.bgCancel()
	}
	s.db.close()
	s.orgs.close()
}

// auditRetentionLoop runs daily: creates next month's audit partition
// idempotently (so the very first INSERT on the first of a new month
// never fails for lack of a target partition) and drops every partition
// whose upper bound is older than cfg.AuditRetentionDays. Setting
// AuditRetentionDays to 0 disables the drop step but the partition
// creation still runs (otherwise inserts would fail at month rollover).
func (s *Server) auditRetentionLoop() {
	// First tick fires after a short delay, clear of startup work; subsequent
	// ticks are 24h apart. No jitter, this being a single instance.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-s.bgCtx.Done():
			return
		case <-timer.C:
		}
		s.runAuditRetention()
		timer.Reset(24 * time.Hour)
	}
}

func (s *Server) runAuditRetention() {
	ctx, cancel := context.WithTimeout(s.bgCtx, 30*time.Second)
	defer cancel()

	// Expired schema imports, swept on the same tick.
	//
	// Not audit data, and not governed by AuditRetentionDays: an import carries
	// its own expires_at, written when it was stored. It shares this loop
	// because both are daily deletions of rows nothing reads, and a second
	// timer for one statement is a second thing to notice has stopped.
	//
	// Reads already exclude expired rows, so a sweep that fails hides nothing —
	// it leaves somebody else's schema on disk, which is the reason to run it.
	if n, err := s.db.deleteExpiredSchemaImports(ctx); err != nil {
		s.log.Error("schema import retention: delete", "err", err)
	} else if n > 0 {
		s.log.Info("schema import retention: deleted expired imports", "count", n)
	}

	// Roll forward: ensure this month and next month exist.
	// See store.migrate for why the anchor is first-of-month, not today.
	now := time.Now().UTC()
	firstOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := s.db.ensureAuditPartition(ctx, firstOfMonth); err != nil {
		s.log.Error("audit retention: ensure current month", "err", err)
		return
	}
	if err := s.db.ensureAuditPartition(ctx, firstOfMonth.AddDate(0, 1, 0)); err != nil {
		s.log.Error("audit retention: ensure next month", "err", err)
		return
	}

	if s.cfg.AuditRetentionDays <= 0 {
		return // retention disabled
	}
	cutoff := now.AddDate(0, 0, -s.cfg.AuditRetentionDays)
	dropped, err := s.db.dropAuditPartitionsOlderThan(ctx, cutoff)
	if err != nil {
		s.log.Error("audit retention: drop", "err", err)
		return
	}
	if len(dropped) > 0 {
		s.log.Info("audit retention: dropped expired partitions",
			"count", len(dropped),
			"cutoff", cutoff.Format("2006-01-02"),
			"names", dropped,
		)
	}

	// Session GC piggy-backs on the same daily tick. Expired sessions
	// don't grant access (getSessionInfo filters by expires_at > NOW)
	// but accumulating dead rows is unnecessary bloat.
	if n, err := s.db.deleteExpiredSessions(ctx); err != nil {
		s.log.Warn("session gc", "err", err)
	} else if n > 0 {
		s.log.Info("session gc: pruned expired", "count", n)
	}

	// Likewise the record of spent assertions. Once an assertion is past its
	// own expiry it is refused on that ground before the replay check is
	// reached, so the row has stopped carrying information — but one is
	// written per sign-in, so without this the table only grows.
	// And unredeemed enrolment tokens, on the same terms.
	//
	// Housekeeping only. An expired token stops working because the statement
	// that spends it says so — this sweep is not what refuses it, and reading
	// this as the enforcement is how a fifteen-minute token ends up redeemable
	// for a day. See spendEnrollToken.
	if n, err := s.db.deleteExpiredEnrollTokens(ctx); err != nil {
		s.log.Warn("enrolment token gc", "err", err)
	} else if n > 0 {
		s.log.Info("enrolment token gc: pruned expired", "count", n)
	}

	if n, err := s.db.deleteSpentAssertions(ctx); err != nil {
		s.log.Warn("spent assertion gc", "err", err)
	} else if n > 0 {
		s.log.Info("spent assertion gc: pruned expired", "count", n)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) buildMux() {
	mux := http.NewServeMux()

	// Sign-in (no session required). One route: trade an assertion issued by
	// Cloud for a session cookie.
	//
	// There is no first-run setup any more. A console has no accounts to
	// create, so nothing distinguishes its first request from its
	// ten-thousandth — Cloud provisions the stack and decides who may reach
	// it.
	mux.HandleFunc("POST /api/auth/exchange", s.handleExchange)

	// Unauthenticated: a browser holding no session cookie is what reads it.
	mux.HandleFunc("GET /api/config", s.handleConfig)

	// Reads a database this organisation points at and returns .atl describing
	// it. The one route that makes this console dial an address it was given;
	// internal/dsnguard decides what it may reach.
	//
	// csrf as well as auth: it is a state-changing POST that costs an outbound
	// connection, so a form on another origin must not be able to start one.
	mux.HandleFunc("POST /api/schema/import", s.auth(s.csrf(s.handleImportSchema)))

	// Reading back what was imported. Both scoped to the session's
	// organisation by the store, and both plain reads, so no csrf.
	mux.HandleFunc("GET /api/schema/imports", s.auth(s.handleListSchemaImports))
	mux.HandleFunc("GET /api/schema/imports/{id}", s.auth(s.handleGetSchemaImport))
	mux.HandleFunc("GET /api/schema/imports/{id}/entities", s.auth(s.handleGetSchemaImportEntities))
	mux.HandleFunc("GET /api/schema/imports/{id}/notes", s.auth(s.handleGetSchemaImportNotes))
	mux.HandleFunc("POST /api/schema/imports/{id}/plan", s.auth(s.csrf(s.handlePlanImport)))
	mux.HandleFunc("POST /api/schema/imports/{id}/apply", s.auth(s.csrf(s.handleApplyImport)))

	// Auth-required endpoints.
	mux.HandleFunc("POST /api/auth/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.auth(s.handleMe))

	// Admin RPC proxies — all auth-required.
	mux.HandleFunc("GET /api/schema", s.auth(s.handleGetMergedSchema))
	mux.HandleFunc("GET /api/schema/canonical", s.auth(s.handleGetCanonicalIR))
	mux.HandleFunc("GET /api/history", s.auth(s.handleGetSchemaHistory))
	mux.HandleFunc("GET /api/history/{version}", s.auth(s.handleGetSchemaVersion))
	mux.HandleFunc("GET /api/diff", s.auth(s.handleDiffSchemaVersions))
	mux.HandleFunc("GET /api/lineage/{entity}", s.auth(s.handleGetEntityLineage))
	// GetEntityOwners returns all entity→caller ownership; no per-entity filter in the RPC.
	mux.HandleFunc("GET /api/owners", s.auth(s.handleGetEntityOwners))
	mux.HandleFunc("GET /api/parked", s.auth(s.handleListParkedObjects))
	mux.HandleFunc("GET /api/health", s.auth(s.handleHealth))

	// The console does not edit schema. `.atl` files are edited in the
	// customer's own git repository and applied by `tide apply`; this console
	// observes, approves and audits.
	//
	// A route that composed a field edit and opened a pull request would be a
	// second authoring path beside that repository, and would need a
	// caller-to-repository mapping in the product's data model.

	// Caller management.
	mux.HandleFunc("GET /api/callers", s.auth(s.handleGetCallers))
	mux.HandleFunc("POST /api/callers", s.auth(s.requireRole("admin", s.csrf(s.handleRegisterCaller))))
	mux.HandleFunc("DELETE /api/callers/{caller}", s.auth(s.requireRole("admin", s.csrf(s.handleRevokeCaller))))
	// Enrolment replaces cert issuance.
	//
	// This mints a single-use token rather than generating a private key and
	// returning it for download: the machine that will hold the key generates
	// it and sends only a CSR. See enroll.go.
	//
	// Sudo as well as admin, which the issuance route did not require. It
	// produces a credential that becomes a caller's identity — the same class
	// as setting the change policy or revoking every caller.
	mux.HandleFunc("POST /api/callers/{caller}/enroll",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleMintEnrollToken)))))
	// Readable by any signed-in user, like the change policy: knowing which
	// callers have been enrolled is how somebody works out why a deploy is
	// failing, and hiding it from non-admins makes that an escalation.
	mux.HandleFunc("GET /api/callers/certs", s.auth(s.handleGetCallerCerts))
	// Caller aliases. Mounted under /api/admin/callers/... originally to avoid
	// a ServeMux ambiguity with PUT /api/callers/repos/{caller} — both would
	// have matched /api/callers/repos/aliases, which Go 1.22+ refuses to
	// register. That route is gone, so the constraint is too; the path stays
	// because it is a published URL the SPA calls and moving it would break
	// every console mid-upgrade for no gain. Same admin+sudo gating as the rest
	// of the /api/admin/... surface (workers, etc).
	mux.HandleFunc("GET /api/admin/callers/{caller}/aliases",
		s.auth(s.requireRole("admin", s.handleGetCallerAliases)))
	mux.HandleFunc("PUT /api/admin/callers/{caller}/aliases",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleSetCallerAliases)))))

	// Schema rollback — mutation, CSRF-protected, admin-only.
	mux.HandleFunc("POST /api/schema/rollback", s.auth(s.requireRole("admin", s.csrf(s.handleRollbackSchema))))
	mux.HandleFunc("POST /api/schema/rollback/preview", s.auth(s.requireRole("admin", s.csrf(s.handlePreviewRollback))))

	// Change policy — which classes of schema change may apply unattended.
	//
	// Reading is open to any signed-in user: a policy nobody but an admin can
	// see is one an engineer discovers by having an apply refused. Writing
	// takes sudo, alongside sign-out-all and revoke-all-callers, because it is
	// the control that decides whether production DDL runs without a human.
	mux.HandleFunc("GET /api/policy", s.auth(s.handleGetChangePolicy))
	mux.HandleFunc("PUT /api/policy",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleSetChangePolicy)))))

	// Schema plans waiting on a human.
	//
	// Reading is open to any signed-in user: a change blocking an engineer's
	// deploy is not a secret from that engineer. Deciding runs through
	// requirePolicyRole, which resolves the role from the plan's class rather
	// than taking a static one — see the note on that middleware.
	//
	// Approve takes sudo, alongside sign-out-all and revoke-all-callers: it is
	// the button that lets production DDL run. Reject does not, because the
	// worst a wrongly-rejected plan costs is a re-plan, and putting a password
	// prompt in front of "no" is how reviewers stop saying it.
	mux.HandleFunc("GET /api/plans", s.auth(s.handleListSchemaPlans))
	mux.HandleFunc("GET /api/plans/{id}", s.auth(s.handleGetSchemaPlan))
	mux.HandleFunc("POST /api/plans/{id}/approve",
		s.auth(s.requirePolicyRole(s.csrf(s.requireSudo(s.handleApproveSchemaPlan)))))
	mux.HandleFunc("POST /api/plans/{id}/reject",
		s.auth(s.requirePolicyRole(s.csrf(s.handleRejectSchemaPlan))))

	// Job queue management.
	mux.HandleFunc("GET /api/jobs/dead", s.auth(s.handleListDeadJobs))
	mux.HandleFunc("GET /api/jobs/{id}", s.auth(s.handleGetJobStatus))
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.auth(s.requireRole("admin", s.csrf(s.handleRetryDeadJob))))

	// Worker-poll dispatcher admin (PR 3). List + Get are read-only for
	// any authenticated user; Drain + Evict require admin role + sudo
	// (mirror of the Settings panel's destructive-action discipline).
	mux.HandleFunc("GET /api/admin/workers", s.auth(s.handleListConnectedWorkers))
	mux.HandleFunc("GET /api/admin/workers/{id}", s.auth(s.handleGetWorkerSession))
	mux.HandleFunc("POST /api/admin/workers/{id}/drain",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleDrainWorker)))))
	mux.HandleFunc("POST /api/admin/workers/{id}/evict",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleEvictWorker)))))

	// Audit log.
	mux.HandleFunc("GET /api/audit", s.auth(s.handleGetAuditLog))

	// Live log tail, read from atlantis's in-process slog ring buffer through
	// the GetLogs admin RPC. Cursor-based: the caller passes ?since=<seq> and
	// gets back any records with a higher sequence number, plus the new
	// last_seq for the next poll.
	//
	// The records are real: handleGetLogs proxies the RPC rather than
	// synthesising a stream for the Health page.
	mux.HandleFunc("GET /api/logs", s.auth(s.handleGetLogs))

	// No user-management routes. Membership and roles belong to the
	// organisation, which lives at Cloud; this console reads what an
	// assertion tells it. Managing operators here would mean two places
	// disagreeing about who may do what, with the console's copy winning by
	// virtue of being the one enforcing it.

	// Settings-page operations.
	mux.HandleFunc("GET /api/instance", s.auth(s.handleInstance))
	mux.HandleFunc("POST /api/auth/sudo", s.auth(s.csrf(s.handleSudo)))
	mux.HandleFunc("POST /api/auth/sign-out-others", s.auth(s.csrf(s.handleSignOutOthers)))
	// Danger-zone — admin + CSRF + sudo (re-auth within sudoTTL).
	mux.HandleFunc("POST /api/auth/sign-out-all",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleSignOutAll)))))
	mux.HandleFunc("POST /api/callers/revoke-all",
		s.auth(s.requireRole("admin", s.csrf(s.requireSudo(s.handleRevokeAllCallers)))))

	// Sandbox — schema-true in-memory testbed. See sandbox.go for the
	// per-user attribution, TTL janitor, and proxy-to-runtime layer.
	s.mountSandbox(mux)

	// SPA — serve the embedded dist/ for all non-API paths.
	mux.HandleFunc("/", s.handleSPA)

	s.mux = mux
}

type contextKey int

const (
	ctxUser         contextKey = iota // *User
	ctxSessionToken                   // string (raw cookie token)
	ctxSudoUntil                      // *time.Time (nil if not in sudo mode)
)

// auth wraps a handler requiring a valid session cookie. 401 if missing or
// expired; the authenticated user is stored in the request context.
//
// Performs sliding renewal: when the session's remaining TTL is below
// sessionTouchThreshold of the full window, it bumps expires_at and
// rewrites the cookie's MaxAge. The threshold keeps the renewal write
// off the hot path (~1 UPDATE per 6h for an active user with 12h TTL).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			jsonError(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		info, err := s.db.getSessionInfo(r.Context(), cookie.Value)
		if errors.Is(err, ErrNotFound) {
			jsonError(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.log.Error("session lookup", "err", err)
			jsonError(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Sliding renewal — see sessionTouchThreshold in store.go.
		remaining := time.Until(info.ExpiresAt)
		if remaining < time.Duration(float64(sessionTTL)*sessionTouchThreshold) {
			if err := s.db.touchSession(r.Context(), cookie.Value); err != nil {
				s.log.Warn("touch session", "err", err)
				// Non-fatal: an authenticated request still proceeds with
				// the existing expiry; only the renewal write failed.
			} else {
				// Rewrite the cookie so the browser's MaxAge matches the
				// new server-side expiry. Cookies without MaxAge are
				// session cookies; this one persists and matches the TTL.
				setSessionCookie(w, cookie.Value, s.cfg.CookieSecure)
			}
		}

		ctx := context.WithValue(r.Context(), ctxUser, info.User)
		ctx = context.WithValue(ctx, ctxSessionToken, cookie.Value)
		ctx = context.WithValue(ctx, ctxSudoUntil, info.SudoUntil)
		next(w, r.WithContext(ctx))
	}
}

// requireSudo gates an authenticated handler on the session being in
// sudo mode (i.e. the user has re-authenticated within sudoTTL). Use on
// destructive admin actions: sign-out-all, revoke-all-callers.
//
// 403 with a stable error code so the SPA knows to prompt for re-auth.
func (s *Server) requireSudo(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		su, _ := r.Context().Value(ctxSudoUntil).(*time.Time)
		if su == nil || !su.After(time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"sudo required","code":"sudo_required"}`))
			return
		}
		next(w, r)
	}
}

// acceptAssertion reads an assertion out of the request, verifies it against
// Cloud, and spends it so it cannot be presented twice.
//
// Both entry points below go through here, and both need every step: the
// exchange that opens a session, and the step-up that grants sudo. Writing it
// once means the two cannot drift into applying different checks to the same
// credential — which is the shape the bug would take, since the weaker of the
// two would still work perfectly for everyone using it legitimately.
//
// Returns the verified claims, or writes the response and returns nil.
func (s *Server) acceptAssertion(w http.ResponseWriter, r *http.Request) *identity.Claims {
	// Per-IP limit. Verification is a signature check rather than a bcrypt
	// round, so it is cheap enough to attempt at volume; the limit bounds how
	// fast one host can push candidate assertions at this console.
	if ok, retry := s.loginLim.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		s.log.Warn("assertion exchange rate-limited", "ip", clientIP(r))
		jsonError(w, "too many attempts; try again shortly", http.StatusTooManyRequests)
		return nil
	}

	var body struct {
		Assertion string `json:"assertion"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return nil
	}
	if body.Assertion == "" {
		jsonError(w, "assertion is required", http.StatusBadRequest)
		return nil
	}

	claims, err := s.cloud.Verify(r.Context(), body.Assertion)
	if errors.Is(err, cloudauth.ErrKeysUnavailable) {
		// Not the caller's fault, and not a credential problem: this console
		// cannot reach Cloud's keys, so it cannot judge the assertion either
		// way. Answering 401 would tell the user to try different
		// credentials, which cannot help.
		s.log.Error("cloud signing keys unavailable", "err", err)
		jsonError(w, "cannot reach the identity provider; try again shortly", http.StatusServiceUnavailable)
		return nil
	}
	if err != nil {
		// One message for every rejection. Distinguishing "expired" from
		// "wrong audience" from "bad signature" tells whoever is holding a
		// token which part to work on next.
		s.log.Warn("assertion rejected", "ip", clientIP(r), "err", err)
		jsonError(w, "invalid assertion", http.StatusUnauthorized)
		return nil
	}

	// Spend it. An assertion travels through a browser and is therefore
	// exposed to anything else running on the page; single use means a copy
	// captured in flight is worth nothing once the legitimate request lands.
	if err := s.db.spendAssertion(r.Context(), claims.ID, claims.Expiry); errors.Is(err, ErrAssertionSpent) {
		s.log.Warn("assertion replayed", "ip", clientIP(r), "subject", claims.Subject)
		jsonError(w, "invalid assertion", http.StatusUnauthorized)
		return nil
	} else if err != nil {
		s.log.Error("record spent assertion", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return nil
	}

	return claims
}

// handleExchange trades a Cloud assertion for a session cookie.
//
// The console is a browser application, so the assertion is spent immediately
// and everything afterwards rides on the existing session cookie. Keeping the
// token in JavaScript to send as a bearer header would put a live credential
// somewhere every script on the page can read it, and would give up the
// SameSite and HttpOnly properties the cookie already has.
func (s *Server) handleExchange(w http.ResponseWriter, r *http.Request) {
	claims := s.acceptAssertion(w, r)
	if claims == nil {
		return
	}

	// Record the organisation before opening the session.
	//
	// Not fatal if it fails: the assertion verified, so the sign-in is
	// legitimate and refusing it over a registry write would take the console
	// down for a bookkeeping problem. The registry is how step 5 finds an
	// organisation's endpoint and certificate, not part of this request's
	// authorisation — the boundary itself is the policy on the data.
	if err := s.db.rememberOrg(r.Context(), claims.Org); err != nil {
		s.log.Warn("record organisation", "org", claims.Org, "err", err)
	}

	token, err := s.db.createSession(r.Context(), claims)
	if err != nil {
		s.log.Error("create session", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Whatever this browser held before, it does not hold now.
	//
	// Setting the cookie below replaces the value, so the previous row becomes
	// unreachable — but it stays valid until it expires, and a session nobody
	// can reach still counts wherever sessions are counted: sign-out-all,
	// "other sessions", any future quota. One browser, one row.
	//
	// After the new session exists, so a failure here leaves somebody signed in
	// rather than signed out of both. Not fatal for the same reason: the
	// sign-in succeeded, and refusing it over a row that will expire anyway
	// would be losing the thing to protect the bookkeeping.
	if prev, err := r.Cookie(sessionCookieName); err == nil && prev.Value != token {
		if err := s.db.deleteSession(r.Context(), prev.Value); err != nil {
			s.log.Warn("delete the session being replaced", "err", err)
		}
	}

	setSessionCookie(w, token, s.cfg.CookieSecure)
	s.db.forOrg(claims.Org).logAction(r.Context(), claims.Subject, claims.Email, "signed_in", map[string]any{
		"org": claims.Org,
	})
	jsonOK(w, map[string]bool{"ok": true})
}

// handleSudo grants the current session a short window of elevated permission
// (sudoTTL ~ 5 minutes). Required by destructive endpoints — sign-out-all,
// revoke-all — so a stolen session cookie alone cannot trigger them.
//
// With no local credentials, step-up means presenting an assertion that says a
// second factor was just presented, which sends the user back to Cloud to prove
// it there. The property is the same either way: holding the cookie is not
// enough.
//
// It requires the StepUp claim, not merely an unspent assertion. Single-use
// prevents replay, not re-minting: a live Cloud session mints a fresh assertion
// on request and lasts twelve hours without a second factor being presented.
//
// Cloud sets StepUp on one route, /authorize with prompt=reauth, and only after
// checking the code. Without the check below the claim is inert and sudo always
// succeeds, which looks identical to a working gate on every screen and in
// every audit row.
func (s *Server) handleSudo(w http.ResponseWriter, r *http.Request) {
	claims := s.acceptAssertion(w, r)
	if claims == nil {
		return
	}

	if !claims.StepUp {
		s.log.Warn("sudo assertion did not come from a step-up", "subject", claims.Subject)
		jsonError(w, "confirm with Atlantis Cloud to continue", http.StatusForbidden)
		return
	}

	u := r.Context().Value(ctxUser).(*User)

	// The assertion must be for the user already holding this session.
	// Without this, anyone with a valid assertion of their own could elevate
	// somebody else's session by posting it against their cookie.
	if claims.Subject != u.Subject {
		s.log.Warn("sudo assertion is for a different subject",
			"session", u.Subject, "assertion", claims.Subject)
		jsonError(w, "assertion does not match this session", http.StatusForbidden)
		return
	}

	token, _ := r.Context().Value(ctxSessionToken).(string)
	if token == "" {
		jsonError(w, "session not found", http.StatusUnauthorized)
		return
	}
	if err := s.db.grantSudo(r.Context(), token); err != nil {
		s.log.Error("grant sudo", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "sudo_granted", map[string]any{
		"ttl_seconds": int(sudoTTL.Seconds()),
	})
	jsonOK(w, map[string]any{"ok": true, "expires_in_seconds": int(sudoTTL.Seconds())})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, _ := r.Cookie(sessionCookieName)
	if cookie != nil {
		_ = s.db.deleteSession(r.Context(), cookie.Value)
	}
	clearSessionCookie(w, s.cfg.CookieSecure)
	jsonOK(w, map[string]bool{"ok": true})
}

// handleConfig returns the addresses the SPA needs before it holds a session.
//
// The URL names no organisation: this console serves several, a request with
// no session identifies none of them, and Cloud's /authorize answers 400
// without one.
//
// The address comes from CloudIssuer, the same configuration handleMe builds
// step_up_url from.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, map[string]any{
		"cloud_signin_url": s.cfg.CloudIssuer + "/signin",
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)
	jsonOK(w, map[string]any{
		"subject": u.Subject,
		"org":     u.Org,
		"email":   u.Email,
		"role":    u.Role,
		"name":    u.Name,

		// Where the browser goes to prove a second factor before a destructive
		// action. Built here rather than in the SPA for two reasons: the org
		// comes from the session rather than from anything the page holds, and
		// the console's own configuration is where Cloud's address lives.
		//
		// CLOUD_ISSUER is Cloud's base URL — issuer.New documents the `iss`
		// value as the issuer's https URL, and the console already requires it
		// to match exactly, so there is no second value that could drift.
		"step_up_url": s.cfg.CloudIssuer + "/authorize?org=" +
			url.QueryEscape(u.Org) + "&prompt=reauth",

		// Cloud itself, for the way back. Both sessions survive following it,
		// so returning here costs one click.
		"cloud_url": s.cfg.CloudIssuer,

		// Where signing out goes once this console has dropped its own session.
		// Clearing the cookie here ends nothing at Cloud: separate origins, and
		// a console can clear no cookie but its own.
		"cloud_signout_url": s.cfg.CloudIssuer + "/logout",

		// What this organisation calls itself, when that is not its name.
		// Omitted otherwise, so the page renders one line rather than the same
		// word twice.
		"org_display_name": u.OrgNames[u.Org],

		// Every organisation this person belongs to, each with the URL that
		// switches to it. Built here for the same two reasons as step_up_url,
		// and one more: a name assembled into a URL by the page is a name the
		// page could have chosen.
		//
		// The URL goes to Cloud, not to the other console. Cloud re-reads the
		// membership, mints, and redirects to wherever that organisation's
		// console actually is — which may not be this one. That is the whole
		// switch, and it is why the console needs no idea where anything else
		// lives.
		"orgs": s.switchTargets(u),
	})
}

// switchTargets pairs each organisation with the URL that moves to it.
//
// The current one is included. A switcher that hid it would have to say what it
// is showing some other way, and a list where the selected item is missing is a
// list somebody has to reason about.
func (s *Server) switchTargets(u *User) []map[string]string {
	// Never nil. An absent list and an empty one mean the same thing to the
	// page — draw no switcher — and `null` in JSON is one more shape for it to
	// handle for no gain.
	out := make([]map[string]string, 0, len(u.Orgs))
	for _, org := range u.Orgs {
		t := map[string]string{
			"name": org,
			"url":  s.cfg.CloudIssuer + "/authorize?org=" + url.QueryEscape(org),
		}
		// Only where it differs. The page falls back to the name, so an entry
		// without one draws exactly as it did before this existed.
		if d := u.OrgNames[org]; d != "" && d != org {
			t["display_name"] = d
		}
		out = append(out, t)
	}
	return out
}

// The admin RPC proxies below forward the request to atlantis and pipe the raw
// JSON response back
// to the browser unchanged. This avoids maintaining duplicate type definitions
// for every admin response struct — the browser receives exactly what the
// admin service returns.

func (s *Server) handleGetMergedSchema(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetMergedSchema(r.Context(), &adminpb.GetMergedSchemaRequest{})
	s.proxyProto(w, "GetMergedSchema", resp, err)
}

func (s *Server) handleGetCanonicalIR(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetCanonicalIR(r.Context(), &adminpb.GetCanonicalIRRequest{})
	s.proxyProto(w, "GetCanonicalIR", resp, err, "ir")
}

func (s *Server) handleGetSchemaHistory(w http.ResponseWriter, r *http.Request) {
	req := &adminpb.GetSchemaHistoryRequest{Limit: int32(intQuery(r, "limit", 50))}
	// `before` and `caller` are server-side filters; forward them when set.
	if before, ok := int64Query(r, "before"); ok {
		req.Before = before
	}
	if caller := r.URL.Query().Get("caller"); caller != "" {
		req.Caller = caller
	}
	if id := r.URL.Query().Get("entity_id"); id != "" {
		req.EntityId = id
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetSchemaHistory(r.Context(), req)
	s.proxyProto(w, "GetSchemaHistory", resp, err)
}

func (s *Server) handleGetSchemaVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version <= 0 {
		jsonError(w, "version must be a positive integer", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetSchemaVersion(r.Context(), &adminpb.GetSchemaVersionRequest{Version: version})
	s.proxyProto(w, "GetSchemaVersion", resp, err, "diff", "ir_snapshot")
}

func (s *Server) handleDiffSchemaVersions(w http.ResponseWriter, r *http.Request) {
	from, fromOK := int64Query(r, "from")
	to, toOK := int64Query(r, "to")
	if !fromOK || !toOK {
		jsonError(w, "from and to are required positive integers", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.DiffSchemaVersions(r.Context(), &adminpb.DiffSchemaVersionsRequest{
		FromVersion: from,
		ToVersion:   to,
	})
	s.proxyProto(w, "DiffSchemaVersions", resp, err, "diff", "from_ir", "to_ir")
}

func (s *Server) handleGetEntityLineage(w http.ResponseWriter, r *http.Request) {
	// Admin RPC expects {"entity_id": "..."}.
	entity := r.PathValue("entity")
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetEntityLineage(r.Context(), &adminpb.GetEntityLineageRequest{EntityId: entity})
	s.proxyProto(w, "GetEntityLineage", resp, err)
}

// handleGetEntityOwners returns all entity→caller ownership.
// GetEntityOwners takes no arguments — it always returns the full set.
func (s *Server) handleGetEntityOwners(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetEntityOwners(r.Context(), &adminpb.GetEntityOwnersRequest{})
	s.proxyProto(w, "GetEntityOwners", resp, err)
}

// handleListParkedObjects reads the register of objects a destructive
// migration parked instead of dropping.
//
// Read-only, like the RPC behind it. Extending a retention window or reaping
// early has data consequences, and stays documented SQL rather than a button,
// so the 30-day window is hard to shorten by accident.
func (s *Server) handleListParkedObjects(w http.ResponseWriter, r *http.Request) {
	// Accepts "1" or "true", so neither obvious spelling silently means false.
	all := r.URL.Query().Get("all")
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.ListParkedObjects(r.Context(), &adminpb.ListParkedObjectsRequest{
		IncludeReaped: all == "1" || all == "true",
		Limit:         200,
	})
	s.proxyProto(w, "ListParkedObjects", resp, err)
}

// handleHealth proxies atlantis's HTTP health endpoints, not an admin RPC.
// Returns the shape the SPA's HealthResponse type expects:
//
//	{ atlantis: {
//	    status, checks,
//	    readyz_code, healthz_code,
//	    started_at, server_version, schema_version,
//	    metrics_series } }
//
// Each chip on the Health page maps to one of these fields:
//
//   - /readyz, /healthz codes come from the upstream HTTP status.
//   - uptime is computed by the SPA from started_at.
//   - version is the schema version reported by /status, not the
//     server build version.
//   - metrics_series is the count of non-comment lines in /metrics.
//
// All atlantis HTTP calls share a tight ProbeTimeout so a wedged
// upstream can't stall the SPA's 1Hz health poll.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	e := s.orgAddrs(w, r)
	if e == nil {
		return
	}
	type checkItem struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}

	healthAddr := e.health
	// HTTPS with this organisation's client certificate. The listener demands
	// one on /status and /metrics; /healthz and /readyz would answer without,
	// but presenting it on all four keeps one client and one failure mode.
	hc := e.healthClient
	probe := func(path string) (int, string) {
		resp, err := hc.Get("https://" + healthAddr + path) //nolint:noctx
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		msg := strings.TrimSpace(string(body))
		if msg == "ok" {
			msg = ""
		}
		return resp.StatusCode, msg
	}

	readyzCode, readyzMsg := probe("/readyz")
	healthzCode, healthzMsg := probe("/healthz")

	checkStatus := func(code int) string {
		if code == http.StatusOK {
			return "healthy"
		}
		return "unhealthy"
	}

	checks := []checkItem{
		{Name: "readyz", Status: checkStatus(readyzCode), Message: readyzMsg},
		{Name: "healthz", Status: checkStatus(healthzCode), Message: healthzMsg},
	}

	overall := "healthy"
	if readyzCode != http.StatusOK || healthzCode != http.StatusOK {
		overall = "unhealthy"
	}

	// /status — uptime + version + schema version. Best-effort: if
	// atlantis is down or pre-this-version, the fields stay zero and
	// the SPA renders an em-dash.
	var startedAt, serverVer string
	var schemaVer int64
	if resp, err := hc.Get("https://" + healthAddr + "/status"); err == nil { //nolint:noctx
		var body struct {
			StartedAt     string `json:"started_at"`
			Version       string `json:"version"`
			SchemaVersion int64  `json:"schema_version"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&body) == nil {
			startedAt = body.StartedAt
			serverVer = body.Version
			schemaVer = body.SchemaVersion
		}
		_ = resp.Body.Close()
	}

	// /metrics series count — every non-comment, non-blank line in
	// the Prometheus text format is one series. Not an exact count, just a
	// stable "N series" surface that moves with the metrics.
	metricsSeries := 0
	if resp, err := hc.Get("https://" + healthAddr + "/metrics"); err == nil { //nolint:noctx
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			metricsSeries++
		}
		_ = resp.Body.Close()
	}

	atl := map[string]any{
		"status":         overall,
		"checks":         checks,
		"readyz_code":    readyzCode,
		"healthz_code":   healthzCode,
		"metrics_series": metricsSeries,
	}
	if startedAt != "" {
		atl["started_at"] = startedAt
	}
	if serverVer != "" {
		atl["server_version"] = serverVer
	}
	if schemaVer > 0 {
		atl["schema_version"] = schemaVer
	}

	jsonOK(w, map[string]any{"atlantis": atl})
}

// requireRole wraps a handler enforcing that the authenticated user has one of
// the given roles. Must be composed inside auth() so ctxUser is populated.
func (s *Server) requireRole(role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := r.Context().Value(ctxUser).(*User)
		if u.Role != role {
			jsonError(w, fmt.Sprintf("forbidden: %s role required", role), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// csrf rejects requests whose Origin header doesn't match the server host.
// Combined with SameSite=Strict session cookies this prevents cross-site
// request forgery on the console's state-changing endpoints.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// Strip scheme, compare host only.
			origin = strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
			if origin != r.Host {
				jsonError(w, "CSRF check failed", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// loginLimiter is a per-IP sliding-window leaky bucket sized for a
// human-paced sign-in.
//
// The console holds no passwords, and verifying an assertion is a signature
// check measured in microseconds, so nothing else bounds how fast one host can
// push candidate assertions at this console.
//
// Memory bound: at most loginLimiterMaxIPs entries, each holding up to
// loginLimiterMax timestamps. ~10KB ceiling under sustained attack.
const (
	loginLimiterMax      = 10          // attempts allowed per window per IP
	loginLimiterWindow   = time.Minute // sliding window
	loginLimiterMaxIPs   = 10_000      // cap to bound memory
	loginLimiterSweepAge = 10 * time.Minute
)

type loginLimiter struct {
	mu      sync.Mutex
	hits    map[string][]time.Time
	lastSwp time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{hits: make(map[string][]time.Time), lastSwp: time.Now()}
}

// allow returns (ok, retryAfter). When ok is false, retryAfter is the
// seconds until the oldest in-window attempt rotates out.
func (l *loginLimiter) allow(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-loginLimiterWindow)

	// Periodic sweep so the map doesn't grow forever after an attack.
	if now.Sub(l.lastSwp) > loginLimiterSweepAge {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(now.Add(-loginLimiterSweepAge)) {
				delete(l.hits, k)
			}
		}
		l.lastSwp = now
	}

	// Hard cap on tracked IPs: a new IP arriving at the cap is refused, rather
	// than entries being amnestied under load.
	if len(l.hits) >= loginLimiterMaxIPs {
		if _, known := l.hits[ip]; !known {
			return false, int(loginLimiterWindow.Seconds())
		}
	}

	// Trim this IP's timestamps to the current window.
	hits := l.hits[ip]
	idx := 0
	for ; idx < len(hits); idx++ {
		if hits[idx].After(cutoff) {
			break
		}
	}
	hits = hits[idx:]

	if len(hits) >= loginLimiterMax {
		l.hits[ip] = hits
		retry := int(loginLimiterWindow.Seconds() - now.Sub(hits[0]).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}

	l.hits[ip] = append(hits, now)
	return true, 0
}

// clientIP returns the best-guess IP for rate-limiting. RemoteAddr is
// the immediate peer (the BFF talks to a reverse-proxy/LB in prod, so
// real-IP headers can be honored when configured); for the self-host
// single-VM case RemoteAddr is the actual client.
func clientIP(r *http.Request) string {
	// X-Forwarded-For is "client, proxy1, proxy2" — first hop is the
	// closest-to-client. Only trust when the deploy explicitly opts in
	// (CONSOLE_TRUST_PROXY=true), otherwise spoofable.
	if os.Getenv("CONSOLE_TRUST_PROXY") == "true" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// withSecurityHeaders wraps a handler so every response carries a
// hardened header set: CSP, Referrer-Policy, X-Frame-Options, etc.
// HSTS only ships when CookieSecure (i.e. HTTPS), since HSTS on plain
// HTTP is a foot-gun.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	// Every stylesheet and font is served from this origin, so the policy
	// carries no third-party origin for either.
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// 'unsafe-inline' for styles is required because Vite's prod build
		// inlines a small style block and React style={{...}} props are used.
		// It is not set for scripts, which is the dangerous one.
		"style-src 'self' 'unsafe-inline'",
		"font-src 'self'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Disable browser APIs the console doesn't use, so an XSS-injected
		// script can't reach for geolocation / mic / camera / payment.
		h.Set("Permissions-Policy",
			"camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if s.cfg.CookieSecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// handleInstance returns small, non-secret runtime facts the Settings
// page renders: the gRPC endpoint callers connect to. Auth-required so
// it's not exposed publicly.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	e := s.orgAddrs(w, r)
	if e == nil {
		return
	}
	jsonOK(w, map[string]any{
		"endpoint": e.endpoint,
	})
}

// Passwords are Cloud's business. There is no local credential to rotate, so
// the console offers no way to change one — a user changes their password
// where their account lives.

// handleSignOutOthers terminates every session for the calling user
// except the one whose token is on the current request. The user stays
// signed in on this device.
func (s *Server) handleSignOutOthers(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		jsonError(w, "no session", http.StatusUnauthorized)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	count, err := s.db.forOrg(u.Org).deleteSessionsForSubjectExcept(r.Context(), u.Subject, cookie.Value)
	if err != nil {
		s.log.Error("sign out others", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "sign_out_others", map[string]any{
		"sessions_removed": count,
	})
	jsonOK(w, map[string]any{"ok": true, "sessions_removed": count})
}

// handleSignOutAll terminates every session for every operator — the
// caller included. Admin-only. The danger-zone confirmation modal on
// the SPA enforces explicit consent before this hits.
func (s *Server) handleSignOutAll(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(*User)
	count, err := s.db.forOrg(actor.Org).deleteAllSessions(r.Context())
	if err != nil {
		s.log.Error("sign out all", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.db.forOrg(actor.Org).logAction(r.Context(), actor.Subject, actor.Email, "sign_out_all", map[string]any{
		"sessions_removed": count,
	})
	clearSessionCookie(w, s.cfg.CookieSecure)
	jsonOK(w, map[string]any{"ok": true, "sessions_removed": count})
}

// handleRevokeAllCallers iterates every known caller and calls the
// existing single-revoke admin RPC for each. This drops them from
// caller_identities so the server stops accepting their certs (the
// certs themselves remain cryptographically valid until expiry; the
// allowlist is the trust gate). Schema files in caller_registrations
// are also removed, matching the single-revoke semantics — the design
// label "Revoke all caller certificates" frames the user-visible
// effect, not the underlying table operation.
//
// Admin-only, CSRF-protected. The SPA additionally requires the user
// to type "revoke all" before allowing the call.
func (s *Server) handleRevokeAllCallers(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(ctxUser).(*User)

	// List all known callers via the existing admin RPC; iterate revokes.
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	list, err := atl.GetCallers(r.Context(), &adminpb.GetCallersRequest{})
	if err != nil {
		s.log.Error("RevokeAll: GetCallers", "err", err)
		jsonError(w, "GetCallers: "+err.Error(), http.StatusBadGateway)
		return
	}

	revoked := 0
	failures := []string{}
	for _, c := range list.GetCallers() {
		if c.GetCaller() == "" {
			continue
		}
		atl := s.orgATL(w, r)
		if atl == nil {
			return
		}
		if _, err := atl.RevokeCaller(r.Context(),
			&adminpb.RevokeCallerRequest{Caller: c.GetCaller()}); err != nil {
			s.log.Warn("RevokeAll: revoke caller", "caller", c.GetCaller(), "err", err)
			failures = append(failures, c.GetCaller())
			continue
		}
		revoked++
	}

	s.db.forOrg(actor.Org).logAction(r.Context(), actor.Subject, actor.Email, "revoke_all_callers", map[string]any{
		"revoked":  revoked,
		"failures": failures,
	})
	jsonOK(w, map[string]any{
		"ok":       true,
		"revoked":  revoked,
		"failures": failures,
	})
}

func (s *Server) handleGetCallers(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetCallers(r.Context(), &adminpb.GetCallersRequest{})
	s.proxyProto(w, "GetCallers", resp, err)
}

func (s *Server) handleRegisterCaller(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Caller    string `json:"caller"`
		CanMutate bool   `json:"can_mutate"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}

	actor := r.Context().Value(ctxUser).(*User)
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.RegisterCaller(r.Context(), &adminpb.RegisterCallerRequest{
		Caller:    body.Caller,
		CanMutate: body.CanMutate,
		CreatedBy: actor.Email,
	})
	if err != nil {
		s.log.Error("RegisterCaller", "caller", body.Caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.forOrg(actor.Org).logAction(r.Context(), actor.Subject, actor.Email, "register_caller", map[string]any{
		"caller":     body.Caller,
		"can_mutate": body.CanMutate,
	})
	s.proxyProto(w, "RegisterCaller", resp, nil)
}

// handleGetCallerAliases proxies the GetCallerAliases admin RPC. Read-
// only; gated by admin role at the route. The atlantis-side returns
// 404-equivalent when the caller isn't registered.
func (s *Server) handleGetCallerAliases(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetCallerAliases(r.Context(), &adminpb.GetCallerAliasesRequest{Caller: caller})
	s.proxyProto(w, "GetCallerAliases", resp, err)
}

// handleSetCallerAliases proxies the SetCallerAliases admin RPC.
// Sudo-gated by the route. Audit-logged with the new alias set so an
// operator review of caller permissions can trace which CN was granted
// which aliases when.
func (s *Server) handleSetCallerAliases(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}
	var body struct {
		Aliases []string `json:"aliases"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.SetCallerAliases(r.Context(), &adminpb.SetCallerAliasesRequest{
		Caller:  caller,
		Aliases: body.Aliases,
	})
	if err != nil {
		s.log.Error("SetCallerAliases", "caller", caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "set_caller_aliases", map[string]any{
		"caller":  caller,
		"aliases": body.Aliases,
	})
	s.proxyProto(w, "SetCallerAliases", resp, nil)
}

func (s *Server) handleRevokeCaller(w http.ResponseWriter, r *http.Request) {
	caller := r.PathValue("caller")
	if caller == "" {
		jsonError(w, "caller is required", http.StatusBadRequest)
		return
	}

	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.RevokeCaller(r.Context(), &adminpb.RevokeCallerRequest{Caller: caller})
	if err != nil {
		s.log.Error("RevokeCaller", "caller", caller, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "revoke_caller", map[string]any{"caller": caller})

	s.proxyProto(w, "admin", resp, nil)
}

func (s *Server) handleRollbackSchema(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToVersion int64 `json:"to_version"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ToVersion <= 0 {
		jsonError(w, "to_version is required", http.StatusBadRequest)
		return
	}

	// The caller is this console, which is what the org server verified on
	// the connection. The person goes in the actor pair beside it, where a
	// value the server cannot authenticate belongs.
	//
	// A rollback is operator-driven and an apply is caller-driven, and both
	// still read apart on inspection — by actor now rather than by a person's
	// address wearing a caller's column.
	u := r.Context().Value(ctxUser).(*User)
	actor, actorEmail := u.Actor()

	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.RollbackSchema(r.Context(), &adminpb.RollbackSchemaRequest{
		ToVersion:  body.ToVersion,
		Caller:     consoleCaller,
		Actor:      actor,
		ActorEmail: actorEmail,
	})
	if err != nil {
		s.log.Error("RollbackSchema", "to_version", body.ToVersion, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "rollback_schema", map[string]any{
		"to_version": body.ToVersion,
		"actor":      actor,
	})

	s.proxyProto(w, "admin", resp, nil)
}

// handlePreviewRollback proxies to the read-only PreviewRollback admin RPC.
// Returns the SQL the rollback would execute + plan class + change count,
// without taking the advisory lock or persisting anything. The user
// reviews this before clicking Execute, which calls the real
// /api/schema/rollback that recomputes from a fresh snapshot.
func (s *Server) handlePreviewRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ToVersion int64 `json:"to_version"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ToVersion <= 0 {
		jsonError(w, "to_version is required", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.PreviewRollback(r.Context(), &adminpb.PreviewRollbackRequest{
		ToVersion: body.ToVersion,
	})
	if err != nil {
		s.log.Error("PreviewRollback", "to_version", body.ToVersion, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	s.proxyProto(w, "admin", resp, nil)
}

func (s *Server) handleListDeadJobs(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	jobName := r.URL.Query().Get("job_name")
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.ListDeadJobs(r.Context(), &adminpb.ListDeadJobsRequest{
		Limit:   int32(limit),
		JobName: jobName,
	})
	s.proxyProto(w, "ListDeadJobs", resp, err, "args")
}

func (s *Server) handleGetJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetJobStatus(r.Context(), &adminpb.GetJobStatusRequest{JobId: id})
	s.proxyProto(w, "GetJobStatus", resp, err, "args")
}

func (s *Server) handleRetryDeadJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.RetryDeadJob(r.Context(), &adminpb.RetryDeadJobRequest{JobId: id})
	if err != nil {
		s.log.Error("RetryDeadJob", "job_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "retry_dead_job", map[string]any{"job_id": id})

	s.proxyProto(w, "admin", resp, nil)
}

func (s *Server) handleListConnectedWorkers(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.ListConnectedWorkers(r.Context(), &adminpb.ListConnectedWorkersRequest{})
	s.proxyProto(w, "ListConnectedWorkers", resp, err)
}

func (s *Server) handleGetWorkerSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetWorkerSession(r.Context(), &adminpb.GetWorkerSessionRequest{SessionId: id})
	s.proxyProto(w, "GetWorkerSession", resp, err)
}

func (s *Server) handleDrainWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.DrainWorker(r.Context(), &adminpb.DrainWorkerRequest{SessionId: id})
	if err != nil {
		s.log.Error("DrainWorker", "session_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "worker_drained", map[string]any{"session_id": id})
	s.proxyProto(w, "admin", resp, nil)
}

func (s *Server) handleEvictWorker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.EvictWorker(r.Context(), &adminpb.EvictWorkerRequest{SessionId: id})
	if err != nil {
		s.log.Error("EvictWorker", "session_id", id, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	u := r.Context().Value(ctxUser).(*User)
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "worker_evicted", map[string]any{"session_id": id})
	s.proxyProto(w, "admin", resp, nil)
}

func (s *Server) handleGetAuditLog(w http.ResponseWriter, r *http.Request) {
	u := r.Context().Value(ctxUser).(*User)
	limit := intQuery(r, "limit", 100)
	entries, err := s.db.forOrg(u.Org).listAuditLog(r.Context(), limit)
	if err != nil {
		s.log.Error("list audit log", "err", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type wire struct {
		ID         int64           `json:"id"`
		Actor      string          `json:"actor"`
		ActorEmail string          `json:"actor_email"`
		Action     string          `json:"action"`
		Detail     json.RawMessage `json:"detail,omitempty"`
		CreatedAt  string          `json:"created_at"`
	}
	out := make([]wire, 0, len(entries))
	for _, e := range entries {
		out = append(out, wire{
			ID:         e.ID,
			Actor:      e.Actor,
			ActorEmail: e.ActorEmail,
			Action:     e.Action,
			Detail:     json.RawMessage(e.Detail),
			CreatedAt:  e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	jsonOK(w, map[string]any{"entries": out})
}

// handleGetLogs proxies the SPA's poll into atlantis's GetLogs admin RPC,
// which reads from the lock-free in-process slog ring buffer (see
// internal/obs/logring.go). Cursor-based: the client passes ?since=N and
// receives records with Seq > N plus the new last_seq for the next poll.

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	limit := intQuery(r, "limit", 0)

	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetLogs(r.Context(), &adminpb.GetLogsRequest{
		Since: since,
		Limit: int32(limit),
	})
	s.proxyProto(w, "GetLogs", resp, err)
}

// handleSPA serves the built console.
//
// The logic moved to internal/spafs when Cloud grew a SPA of its own and needed
// the identical three behaviours. Two copies would have meant a fix in one
// silently missing from the other; the move also brought this path its first
// tests, a Cache-Control policy, and two defects fixed — see that package.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	spafs.Handler(s.spaFS, "console SPA not built yet — run: make build-console-spa").
		ServeHTTP(w, r)
}

// proxyProto writes an admin RPC response to w as canonical proto JSON.
//
// Each RPC is a typed call on the generated client rather than a method name
// string, so what is shared between them is the handling of the result.
//
// The dialect is clients/go/adminjson, the same one tide and tidectl emit.
// Empty lists arrive as [] rather than null (protobuf cannot distinguish empty
// from absent, and a browser that iterates the field should not have to), while
// proto3 `optional` fields left unset stay absent so "not set" remains
// distinguishable from "set to zero".
//
// inlineJSONBytes names `bytes` fields on this response that hold a JSON
// document rather than opaque octets; without it they render as base64. See
// adminjson.MarshalIndentInlining.
func (s *Server) proxyProto(w http.ResponseWriter, name string, resp proto.Message, err error, inlineJSONBytes ...string) {
	if err != nil {
		s.log.Error("admin rpc", "method", name, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	b, merr := adminjson.MarshalIndentInlining(resp, inlineJSONBytes...)
	if merr != nil {
		s.log.Error("admin rpc: marshal response", "method", name, "err", merr)
		jsonError(w, merr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// shortPlanClass lived here. It lowercased PLAN_CLASS_BACKFILL_REQUIRED to
// "backfill_required" for the one response this BFF assembled itself — the
// schema-edit preview, now removed.
//
// Its absence changes what the browser sees. Every plan_class the console
// serves is now proxied straight from the admin server, and every one of those
// is written by codegen.ChangeClass.String(), which spells the same classes
// with HYPHENS: "backfill-required", "cross-caller-breaking". There is no
// longer a second spelling in flight.
//
// The SPA still normalises separators (see planClassBadge in
// web/console/src/api/client.ts) because its lookup tables are keyed with
// underscores, and because a proxied string is not something this BFF controls.

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// jsonErrorCode answers with a machine-readable reason beside the sentence.
//
// For a refusal the browser must act on rather than print. The message still
// stands on its own: a client that ignores the code shows a sentence that
// makes sense.
func jsonErrorCode(w http.ResponseWriter, msg, reason string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": reason})
}

func readJSON(r *http.Request, dst any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}

func intQuery(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}

// int64Query parses a positive int64 query parameter. The second return
// value is true only when the param was present and parsed successfully —
// used for optional filters where "missing" must be distinguished from
// "zero", so before=0 never reaches the server by accident.
func int64Query(r *http.Request, key string) (int64, bool) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// handleGetChangePolicy proxies the deployment's change policy.
func (s *Server) handleGetChangePolicy(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetChangePolicy(r.Context(), &adminpb.GetChangePolicyRequest{})
	s.proxyProto(w, "GetChangePolicy", resp, err)
}

// handleSetChangePolicy writes the rules the request names and leaves the rest
// alone.
//
// updated_by is the signed-in console user, taken from the session rather than
// from the request body. A body field would let the caller write any name into
// an audit column, which is worse than having no column at all: it would read
// as evidence.
func (s *Server) handleSetChangePolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entries []struct {
			ChangeClass     string `json:"change_class"`
			RequireApproval bool   `json:"require_approval"`
			ApproverRole    string `json:"approver_role"`
		} `json:"entries"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(body.Entries) == 0 {
		jsonError(w, "no policy entries to set", http.StatusBadRequest)
		return
	}

	entries := make([]*adminpb.ChangePolicyEntry, 0, len(body.Entries))
	for _, e := range body.Entries {
		// The class arrives as the proto enum's name, which is how the server
		// stores it and how GetChangePolicy renders it. Resolved here rather
		// than passed through as a number so an unknown name is refused at the
		// edge with something a person can read.
		num, ok := adminpb.PlanClass_value[e.ChangeClass]
		if !ok {
			jsonError(w, "unknown change class: "+e.ChangeClass, http.StatusBadRequest)
			return
		}
		entries = append(entries, &adminpb.ChangePolicyEntry{
			ChangeClass:     adminpb.PlanClass(num),
			RequireApproval: e.RequireApproval,
			ApproverRole:    e.ApproverRole,
		})
	}

	u := r.Context().Value(ctxUser).(*User)
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.SetChangePolicy(r.Context(), &adminpb.SetChangePolicyRequest{
		Entries:   entries,
		UpdatedBy: u.Email,
	})
	if err != nil {
		s.log.Error("SetChangePolicy", "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	changed := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		changed = append(changed, map[string]any{
			"change_class":     e.GetChangeClass().String(),
			"require_approval": e.GetRequireApproval(),
			"approver_role":    e.GetApproverRole(),
		})
	}
	s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, "set_change_policy", map[string]any{"entries": changed})
	s.proxyProto(w, "SetChangePolicy", resp, nil)
}

// ctxPlanRole carries the role requirePolicyRole resolved, so the handler
// behind it asserts the same role the middleware checked rather than looking it
// up again and possibly getting a different answer.
type ctxPlanRoleKey struct{}

// requirePolicyRole gates a decision on the role the plan's class requires.
//
// requireRole cannot do this. It takes a static role string, decided when the
// route is registered — and the role that may approve is per-class, stored in
// atlantis.change_policy, and only knowable after the plan has been loaded and
// its class read. Wiring requireRole("admin") here hard-codes the default and
// ignores a change_policy row naming a different role for destructive changes.
//
// The plan is fetched once here and the resolved role is stashed in the request
// context, the way auth already stashes ctxUser. The handler then asserts that
// role to the server, which checks it against the same policy — so this
// middleware being wrong produces a refusal there rather than a decision
// nobody was entitled to make.
func (s *Server) requirePolicyRole(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID := r.PathValue("id")
		if planID == "" {
			jsonError(w, "plan id is required", http.StatusBadRequest)
			return
		}
		atl := s.orgATL(w, r)
		if atl == nil {
			return
		}
		resp, err := atl.GetSchemaPlan(r.Context(), &adminpb.GetSchemaPlanRequest{PlanId: planID})
		if err != nil {
			s.log.Error("GetSchemaPlan", "plan", planID, "err", err)
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		want := resp.GetPlan().GetSummary().GetApproverRole()
		u := r.Context().Value(ctxUser).(*User)
		if u.Role != want {
			jsonError(w, fmt.Sprintf("forbidden: %s role required to decide this change", want),
				http.StatusForbidden)
			return
		}
		// The user's role travels onward.
		//
		// Forwarding `want` instead is equivalent: the check above refuses
		// unless u.Role == want, so past this line the two are the same string,
		// and swapping them changes no observable behaviour. u.Role says what
		// it means rather than resting on an invariant three lines up.
		//
		// What protects the server is that a request which never passed through
		// here carries no role at all. The handler reads the empty string,
		// asserts it, and the server refuses, a class's approver_role never
		// being "". So removing or reordering this middleware fails the
		// legitimate path loudly rather than opening the illegitimate one
		// quietly, and TestApproveSucceedsWithRoleSudoAndOrigin is what fails.
		next(w, r.WithContext(context.WithValue(r.Context(), ctxPlanRoleKey{}, u.Role)))
	}
}

// handleListSchemaPlans proxies the approval queue.
func (s *Server) handleListSchemaPlans(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.ListSchemaPlans(r.Context(), &adminpb.ListSchemaPlansRequest{
		State:  r.URL.Query().Get("state"),
		Caller: r.URL.Query().Get("caller"),
	})
	s.proxyProto(w, "ListSchemaPlans", resp, err)
}

// handleGetSchemaPlan proxies one plan, including the proposed .atl source.
func (s *Server) handleGetSchemaPlan(w http.ResponseWriter, r *http.Request) {
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.GetSchemaPlan(r.Context(), &adminpb.GetSchemaPlanRequest{
		PlanId: r.PathValue("id"),
	})
	s.proxyProto(w, "GetSchemaPlan", resp, err)
}

func (s *Server) handleApproveSchemaPlan(w http.ResponseWriter, r *http.Request) {
	s.decideSchemaPlan(w, r, true)
}

func (s *Server) handleRejectSchemaPlan(w http.ResponseWriter, r *http.Request) {
	s.decideSchemaPlan(w, r, false)
}

// decideSchemaPlan records a console user's decision.
//
// decided_by comes from the session, never from the body. A body field would
// let the caller write any name into an audit column, which is worse than
// having no column at all — it would read as evidence.
func (s *Server) decideSchemaPlan(w http.ResponseWriter, r *http.Request, approve bool) {
	planID := r.PathValue("id")
	var body struct {
		Reason string `json:"reason"`
	}
	if err := readJSON(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	u := r.Context().Value(ctxUser).(*User)
	role, _ := r.Context().Value(ctxPlanRoleKey{}).(string)

	// The two arms are kept whole rather than reduced to a shared variable of
	// interface type: proxyProto takes a proto.Message, and widening to `any` to
	// share one call site costs the type check that keeps a nil-typed response
	// from reaching the encoder.
	audit := func(verb string) {
		s.db.forOrg(u.Org).logAction(r.Context(), u.Subject, u.Email, verb, map[string]any{
			"plan_id": planID,
			"role":    role,
			"reason":  body.Reason,
		})
	}
	if approve {
		atl := s.orgATL(w, r)
		if atl == nil {
			return
		}
		resp, err := atl.ApproveSchemaPlan(r.Context(), &adminpb.ApproveSchemaPlanRequest{
			PlanId: planID, DecidedBy: u.Email, DecidedByRole: role, Reason: body.Reason,
		})
		if err != nil {
			s.log.Error("ApproveSchemaPlan", "plan", planID, "err", err)
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		audit("approve_schema_plan")
		s.proxyProto(w, "ApproveSchemaPlan", resp, nil)
		return
	}
	atl := s.orgATL(w, r)
	if atl == nil {
		return
	}
	resp, err := atl.RejectSchemaPlan(r.Context(), &adminpb.RejectSchemaPlanRequest{
		PlanId: planID, DecidedBy: u.Email, DecidedByRole: role, Reason: body.Reason,
	})
	if err != nil {
		s.log.Error("RejectSchemaPlan", "plan", planID, "err", err)
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	audit("reject_schema_plan")
	s.proxyProto(w, "RejectSchemaPlan", resp, nil)
}
