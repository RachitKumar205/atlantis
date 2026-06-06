package console

// Sandbox console integration — wraps the in-process *sandbox.Server
// with per-user attribution, ephemeral TTL, a startup tempdir sweep,
// and the BFF-custom endpoints (boot/list/destroy) that the runtime's
// HTTP control plane intentionally excludes.
//
// Architecture:
//
//   - Sub-routes under /api/sandbox/{pubID}/* PROXY to the runtime's
//     /v1/sandbox/{internalID}/* after the BFF rewrites the path.
//   - POST /api/sandbox (boot), GET /api/sandbox (list),
//     DELETE /api/sandbox/{pubID} are BFF-custom handlers; the runtime
//     does not expose listing.
//   - PubIDs are 128-bit opaque (crypto/rand) and never leak the
//     runtime's enumerable int36 ids to the browser.
//   - Meta map is guarded by sync.RWMutex; TTL janitor reads under
//     RLock to collect candidates, then evicts under write-lock per id.
//   - "Activity" for TTL = any sub-route request. GET /api/sandbox
//     (the list poll) does NOT touch lastActive.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox"
)

// defaultSandboxTTL is the idle window before the janitor evicts a
// sandbox. Overridable via SANDBOX_TTL (config.go parses the env into
// Config.SandboxTTL).
const defaultSandboxTTL = 30 * time.Minute

// defaultSandboxPerUserLimit caps how many active sandboxes one user
// can hold. Tunable via SANDBOX_PER_USER_LIMIT.
const defaultSandboxPerUserLimit = 3

// maxSnapshotBytes caps the PUT body for /snapshot uploads, preventing
// OOM-via-upload.
const maxSnapshotBytes = 256 << 20 // 256 MiB

// sandboxMeta is the BFF-side bookkeeping for one active sandbox. The
// runtime itself doesn't know about owners or TTL — that's the BFF's
// job. internalID is the runtime's enumerable id; pubID is the
// 128-bit opaque token we hand to the browser.
type sandboxMeta struct {
	pubID         string
	internalID    string
	ownerID       int64
	createdAt     time.Time
	lastActive    time.Time
	schemaVersion string
	backend       string // "sim" | "embedded"
	bootMs        int64
}

// sandboxLayer is the BFF's sandbox subsystem. It owns the singleton
// *sandbox.Server and the per-user meta map.
type sandboxLayer struct {
	srv     *sandbox.Server
	mu      sync.RWMutex
	byPub   map[string]*sandboxMeta
	perUser int
	ttl     time.Duration
}

// newSandboxLayer constructs the layer with provided limits. Limits ≤ 0
// fall back to the defaults declared above.
func newSandboxLayer(perUser int, ttl time.Duration) *sandboxLayer {
	if perUser <= 0 {
		perUser = defaultSandboxPerUserLimit
	}
	if ttl <= 0 {
		ttl = defaultSandboxTTL
	}
	return &sandboxLayer{
		srv:     sandbox.NewServer(),
		byPub:   map[string]*sandboxMeta{},
		perUser: perUser,
		ttl:     ttl,
	}
}

// lookup returns meta + ownership-check result. Called from the proxy
// path; if ok=false the caller returns 404 (we don't distinguish
// "not yours" from "doesn't exist" to avoid pubID enumeration leaks).
func (l *sandboxLayer) lookup(pubID string, ownerID int64) (*sandboxMeta, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m, ok := l.byPub[pubID]
	if !ok {
		return nil, false
	}
	if m.ownerID != ownerID {
		return nil, false
	}
	return m, true
}

// touchActivity bumps lastActive. Called from the proxy on every
// sub-route request (NOT from the list endpoint — that would defeat
// TTL eviction for tab-with-list-open-but-otherwise-idle users).
func (l *sandboxLayer) touchActivity(pubID string) {
	l.mu.Lock()
	if m, ok := l.byPub[pubID]; ok {
		m.lastActive = time.Now()
	}
	l.mu.Unlock()
}

// listForUser returns the user's active sandboxes, ordered by createdAt
// descending (most recent first). RLock-only; cheap.
func (l *sandboxLayer) listForUser(ownerID int64) []*sandboxMeta {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []*sandboxMeta
	for _, m := range l.byPub {
		if m.ownerID == ownerID {
			out = append(out, m)
		}
	}
	// Tiny manual sort; not worth importing sort for a typically-3-entry slice.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].createdAt.Before(out[j].createdAt); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// countForUser is the per-user-limit check helper.
func (l *sandboxLayer) countForUser(ownerID int64) int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, m := range l.byPub {
		if m.ownerID == ownerID {
			n++
		}
	}
	return n
}

// register inserts a new sandbox into the meta map and returns its
// pubID. Caller has already invoked sandbox.New and srv.Register.
func (l *sandboxLayer) register(meta *sandboxMeta) {
	l.mu.Lock()
	l.byPub[meta.pubID] = meta
	l.mu.Unlock()
}

// destroy evicts the sandbox by pubID: closes the runtime Sandbox,
// unregisters from the runtime's internal map, and removes the meta.
// Idempotent — destroying an already-gone sandbox is a no-op.
func (l *sandboxLayer) destroy(pubID string) {
	l.mu.Lock()
	m, ok := l.byPub[pubID]
	if !ok {
		l.mu.Unlock()
		return
	}
	delete(l.byPub, pubID)
	internal := m.internalID
	l.mu.Unlock()
	// Close / Unregister outside the lock so a slow embedded shutdown
	// doesn't stall a concurrent listForUser RLock.
	l.srv.Unregister(internal)
}

// runJanitor walks the meta map every interval and evicts sandboxes
// whose lastActive is older than the TTL. Returns when ctx is cancelled.
func (l *sandboxLayer) runJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-l.ttl)
		// Collect candidate ids under RLock — fast read pass.
		var stale []string
		l.mu.RLock()
		for pub, m := range l.byPub {
			if m.lastActive.Before(cutoff) {
				stale = append(stale, pub)
			}
		}
		l.mu.RUnlock()
		// Evict one at a time. destroy() takes the write-lock briefly
		// per call; we hold no aggregate lock so concurrent boots
		// during eviction don't queue up behind a long Close().
		for _, pub := range stale {
			l.destroy(pub)
		}
	}
}

// mintPubID returns a 128-bit random hex string. Used both for the
// browser-facing sandbox id (`pubID`) and indirectly prevents the
// runtime's enumerable int36 ids from leaking.
func mintPubID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sweepEmbeddedTempdirs runs once at BFF startup, removing per-sandbox
// embedded-postgres temp directories left over from a prior process
// crash. Matches the layout in
// internal/runtime/sandbox/embedded/embedded.go (atlantis-sandbox-data-*
// and atlantis-sandbox-runtime-* prefixes under os.TempDir()).
//
// Best-effort: logs errors via the provided logger but never fails
// startup. The OS's own /tmp cleanup is unreliable on macOS (3+ days,
// boot-only) and slow on Linux (systemd-tmpfiles default 10 days), so
// this sweep is the actual cleanup path.
func sweepEmbeddedTempdirs(logf func(string, ...any)) {
	dir := os.TempDir()
	patterns := []string{"atlantis-sandbox-data-*", "atlantis-sandbox-runtime-*"}
	for _, p := range patterns {
		matches, err := filepath.Glob(filepath.Join(dir, p))
		if err != nil {
			logf("sandbox startup sweep: glob %s: %v", p, err)
			continue
		}
		for _, m := range matches {
			if err := os.RemoveAll(m); err != nil {
				logf("sandbox startup sweep: remove %s: %v", m, err)
			}
		}
	}
}

// ─────────────────────────── HTTP handlers ───────────────────────────

// mountSandbox registers the BFF's /api/sandbox/* routes on mux. The
// passed user-context middleware (auth + csrf) is already applied at
// the registration level — handlers here see the User via ctxUser.
func (s *Server) mountSandbox(mux *http.ServeMux) {
	// BFF-custom endpoints.
	mux.HandleFunc("POST /api/sandbox", s.auth(s.csrf(s.handleSandboxBoot)))
	mux.HandleFunc("GET /api/sandbox", s.auth(s.handleSandboxList))
	mux.HandleFunc("DELETE /api/sandbox/{pubID}", s.auth(s.csrf(s.handleSandboxDestroy)))

	// Proxy sub-routes. Wildcards in net/http 1.22+ let us match
	// `/{pubID}/{rest...}` with `{path...}` capture. Phase 1 mounts
	// each verb-path pair explicitly so the routing table is easy
	// to audit; a wildcard mount would also work but obscures the
	// surface.
	mux.HandleFunc("POST /api/sandbox/{pubID}/sql/exec", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/sql/query", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("GET /api/sandbox/{pubID}/inspect/catalog", s.auth(s.handleSandboxProxy))
	mux.HandleFunc("GET /api/sandbox/{pubID}/inspect/describe", s.auth(s.handleSandboxProxy))
	mux.HandleFunc("GET /api/sandbox/{pubID}/inspect/sample", s.auth(s.handleSandboxProxy))
	mux.HandleFunc("POST /api/sandbox/{pubID}/inspect/find", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/inspect/diff", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("GET /api/sandbox/{pubID}/snapshot", s.auth(s.handleSandboxProxy))
	mux.HandleFunc("PUT /api/sandbox/{pubID}/snapshot", s.auth(s.csrf(s.handleSandboxSnapshotPut)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/mark", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/restore", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/fixtures/bulk", s.auth(s.csrf(s.handleSandboxProxy)))
	mux.HandleFunc("POST /api/sandbox/{pubID}/fork", s.auth(s.csrf(s.handleSandboxFork)))
}

// sandboxBootRequest carries the browser's boot intent. Backend is
// "sim" or "embedded"; an empty string defaults to sim.
type sandboxBootRequest struct {
	Backend     string `json:"backend"`
	Determinism string `json:"determinism,omitempty"` // "off" | "strict"
	Seed        int64  `json:"seed,omitempty"`
}

type sandboxBootResponse struct {
	PubID         string `json:"pub_id"`
	Backend       string `json:"backend"`
	BootMs        int64  `json:"boot_ms"`
	SchemaVersion string `json:"schema_version,omitempty"`
	EntityCount   int    `json:"entity_count"`
}

// handleSandboxBoot resolves the current IR via GetCanonicalIR, builds
// a *Sandbox, registers it on the runtime, mints a pubID, and inserts
// into the meta map. Per-user limit applies; embedded is opt-in.
func (s *Server) handleSandboxBoot(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(ctxUser).(*User)
	if s.sandboxes.countForUser(user.ID) >= s.sandboxes.perUser {
		jsonError(w, fmt.Sprintf("max %d active sandboxes — destroy one first", s.sandboxes.perUser), http.StatusTooManyRequests)
		return
	}
	var req sandboxBootRequest
	if err := readJSON(r, &req); err != nil {
		// Empty body is OK — treat as default (sim, no determinism).
		req = sandboxBootRequest{}
	}
	backend := sandbox.BackendSim
	switch strings.ToLower(req.Backend) {
	case "", "sim":
		backend = sandbox.BackendSim
	case "embedded":
		backend = sandbox.BackendEmbedded
	default:
		jsonError(w, "backend must be sim or embedded", http.StatusBadRequest)
		return
	}
	determinism := sandbox.DeterminismOff
	if strings.ToLower(req.Determinism) == "strict" {
		determinism = sandbox.DeterminismStrict
	}

	// Resolve the current IR through the existing admin RPC client.
	// GetCanonicalIR returns { IR: json.RawMessage, ContentHash: string }.
	var ir dsl.IR
	var hash string
	if err := s.bootResolveIR(r.Context(), &ir, &hash); err != nil {
		jsonError(w, "resolve current IR: "+err.Error(), http.StatusBadGateway)
		return
	}

	bootStart := time.Now()
	sb, err := sandbox.New(sandbox.Options{
		IR:          &ir,
		Backend:     backend,
		Seed:        req.Seed,
		Determinism: determinism,
	})
	if err != nil {
		jsonError(w, "boot sandbox: "+err.Error(), http.StatusBadRequest)
		return
	}
	bootMs := time.Since(bootStart).Milliseconds()

	internalID := s.sandboxes.srv.Register(sb)
	pubID, err := mintPubID()
	if err != nil {
		// On the off-chance crypto/rand fails, undo the runtime registration
		// to avoid an orphan.
		s.sandboxes.srv.Unregister(internalID)
		_ = sb.Close()
		jsonError(w, "mint pub id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	meta := &sandboxMeta{
		pubID:         pubID,
		internalID:    internalID,
		ownerID:       user.ID,
		createdAt:     now,
		lastActive:    now,
		schemaVersion: hash,
		backend:       string(backend),
		bootMs:        bootMs,
	}
	s.sandboxes.register(meta)

	jsonOK(w, sandboxBootResponse{
		PubID:         pubID,
		Backend:       string(backend),
		BootMs:        bootMs,
		SchemaVersion: hash,
		EntityCount:   len(ir.Entities),
	})
}

// bootResolveIR centralizes the GetCanonicalIR call so handleSandboxBoot
// reads cleanly. The admin server returns { ir, content_hash } per
// admin.go; we forward both into sandbox.Options + the meta map's
// schemaVersion field.
func (s *Server) bootResolveIR(ctx context.Context, ir *dsl.IR, hash *string) error {
	var resp struct {
		IR          json.RawMessage `json:"ir"`
		ContentHash string          `json:"content_hash"`
	}
	if err := s.atl.invoke(ctx, "/atlantis.admin.v1.Admin/GetCanonicalIR", struct{}{}, &resp); err != nil {
		return err
	}
	// dsl.DecodeJSONIR is the canonical decoder; it handles the
	// field-name casing differences across producers.
	decoded, err := dsl.DecodeJSONIR(resp.IR)
	if err != nil {
		return fmt.Errorf("decode IR: %w", err)
	}
	*ir = *decoded
	*hash = resp.ContentHash
	return nil
}

type sandboxListEntry struct {
	PubID         string `json:"pub_id"`
	Backend       string `json:"backend"`
	SchemaVersion string `json:"schema_version,omitempty"`
	CreatedAt     string `json:"created_at"`
	LastActive    string `json:"last_active"`
	BootMs        int64  `json:"boot_ms"`
}

type sandboxListResponse struct {
	Sandboxes []sandboxListEntry `json:"sandboxes"`
}

// handleSandboxList returns the user's active sandboxes. This endpoint
// does NOT touch lastActive — the TTL janitor needs to be able to
// evict sandboxes from a tab that's polling the list but doing nothing
// else.
func (s *Server) handleSandboxList(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(ctxUser).(*User)
	metas := s.sandboxes.listForUser(user.ID)
	out := sandboxListResponse{Sandboxes: make([]sandboxListEntry, 0, len(metas))}
	for _, m := range metas {
		out.Sandboxes = append(out.Sandboxes, sandboxListEntry{
			PubID:         m.pubID,
			Backend:       m.backend,
			SchemaVersion: m.schemaVersion,
			CreatedAt:     m.createdAt.UTC().Format(time.RFC3339),
			LastActive:    m.lastActive.UTC().Format(time.RFC3339),
			BootMs:        m.bootMs,
		})
	}
	jsonOK(w, out)
}

// handleSandboxDestroy tears down a sandbox by pubID. Owner check
// enforced; non-owners see a 404 (same as not-found) to avoid leaking
// "this id exists but isn't yours".
func (s *Server) handleSandboxDestroy(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(ctxUser).(*User)
	pubID := r.PathValue("pubID")
	if _, ok := s.sandboxes.lookup(pubID, user.ID); !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	s.sandboxes.destroy(pubID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSandboxProxy is the generic passthrough to the runtime's
// /v1/sandbox/{internalID}/... routes. Steps:
//
//  1. Resolve pubID → meta + owner check (404 on miss).
//  2. Touch lastActive (TTL reset).
//  3. Rewrite the path to /v1/sandbox/{internalID}/{rest...}.
//  4. Forward to s.sandboxes.srv.Handler().
//
// The runtime's own handler does all the actual work — body parsing,
// SQL execution, marks, fixtures, snapshot, etc. — and sets the
// `t_server_us` field in its responses.
func (s *Server) handleSandboxProxy(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(ctxUser).(*User)
	pubID := r.PathValue("pubID")
	meta, ok := s.sandboxes.lookup(pubID, user.ID)
	if !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	s.sandboxes.touchActivity(pubID)

	// Rewrite path. Original: /api/sandbox/{pubID}/{rest...}.
	// Target: /v1/sandbox/{internalID}/{rest...}.
	const apiPrefix = "/api/sandbox/"
	const runtimePrefix = "/v1/sandbox/"
	if !strings.HasPrefix(r.URL.Path, apiPrefix) {
		jsonError(w, "internal routing error", http.StatusInternalServerError)
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, apiPrefix+pubID)
	r2 := r.Clone(r.Context())
	r2.URL.Path = runtimePrefix + meta.internalID + tail
	s.sandboxes.srv.Handler().ServeHTTP(w, r2)
}

// handleSandboxSnapshotPut is the proxy variant for PUT /snapshot —
// wraps r.Body in http.MaxBytesReader so the runtime side's
// io.ReadAll can't be tricked into eating a 10 GB blob.
func (s *Server) handleSandboxSnapshotPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSnapshotBytes)
	s.handleSandboxProxy(w, r)
}

// sandboxForkRequest is the wire shape; n caps at parentLimitHeadroom
// so a single fork can't burn through a user's remaining sandbox slots.
type sandboxForkRequest struct {
	N int `json:"n"`
}

type sandboxForkResponse struct {
	IDs      []string `json:"ids"`
	BackendK string   `json:"backend"`
}

// handleSandboxFork implements the ownership-propagating Fork. Rather
// than proxy to /v1/sandbox/{id}/fork (which would auto-register
// children under runtime-mint ids the BFF doesn't know about), we
// call Sandbox.Fork directly via the runtime accessor and mint pubIDs
// for each child at the same point as the parent's bookkeeping.
//
// Per-user limit check applies to the parent + total active children;
// asking for more children than slots remain returns 429 before any
// fork happens.
func (s *Server) handleSandboxFork(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	user := r.Context().Value(ctxUser).(*User)
	pubID := r.PathValue("pubID")
	parent, ok := s.sandboxes.lookup(pubID, user.ID)
	if !ok {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	var req sandboxForkRequest
	if err := readJSON(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.N <= 0 {
		jsonError(w, "n must be > 0", http.StatusBadRequest)
		return
	}

	// Per-user budget: existing count + N must stay within the limit.
	if s.sandboxes.countForUser(user.ID)+req.N > s.sandboxes.perUser {
		jsonError(w,
			fmt.Sprintf("fork would exceed per-user limit (have %d, want %d more, cap %d)",
				s.sandboxes.countForUser(user.ID), req.N, s.sandboxes.perUser),
			http.StatusTooManyRequests)
		return
	}

	// Embedded sandboxes cannot fork — the runtime documents this with
	// ErrFeatureRequiresSim. Surface the constraint up-front so the UI
	// can route the user to picking a sim sandbox first.
	if parent.backend == string(sandbox.BackendEmbedded) {
		jsonError(w, "embedded backend cannot fork (sim-only feature)", http.StatusBadRequest)
		return
	}

	parentSB := s.sandboxes.srv.Sandbox(parent.internalID)
	if parentSB == nil {
		// Shouldn't happen — meta map said the sandbox exists but the
		// runtime doesn't know it. Treat as 404 (no inconsistency leak
		// in the error message).
		jsonError(w, "not found", http.StatusNotFound)
		return
	}

	kids, err := parentSB.Fork(req.N)
	if err != nil {
		jsonError(w, "fork: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Register children and mint pubIDs. If anything fails halfway, we
	// best-effort tear down the partial state — better than leaking
	// runtime-registered children the BFF can't reach.
	ids := make([]string, 0, len(kids))
	now := time.Now()
	for i, k := range kids {
		internalID := s.sandboxes.srv.Register(k)
		newPub, err := mintPubID()
		if err != nil {
			// Rollback: close + unregister the kids we just minted, plus
			// the runtime-registered one that didn't get a pubID.
			s.sandboxes.srv.Unregister(internalID)
			_ = k.Close()
			for _, p := range ids {
				s.sandboxes.destroy(p)
			}
			// Also close any later children that Fork already produced
			// but we haven't registered.
			for _, kk := range kids[i+1:] {
				_ = kk.Close()
			}
			jsonError(w, "mint pub id: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.sandboxes.register(&sandboxMeta{
			pubID:         newPub,
			internalID:    internalID,
			ownerID:       user.ID,
			createdAt:     now,
			lastActive:    now,
			schemaVersion: parent.schemaVersion,
			backend:       parent.backend,
			bootMs:        0, // forks are sub-microsecond; don't lie with a synthetic number
		})
		ids = append(ids, newPub)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ids":         ids,
		"backend":     parent.backend,
		"t_server_us": time.Since(start).Microseconds(),
	})
}
