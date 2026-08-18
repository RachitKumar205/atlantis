package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// orgClientRefresh is how long a cached client is used before its row is
// re-read.
//
// It bounds how long a rotated certificate takes to come into use, and how long
// a de-provisioned organisation keeps working. Five minutes matches the JWKS
// cache next door (internal/console/cloudauth/keycache.go) for the same reason:
// the thing being watched changes on the order of months, and the cost of
// checking is one indexed primary-key read.
const orgClientRefresh = 5 * time.Minute

// orgClients holds one admin channel per organisation.
//
// # Why this exists
//
// The console serves many organisations, and each has its own atlantis behind
// its own CA. A single shared client would reach one of them and silently
// return its data to everybody — which is not a hypothetical: it is exactly
// what the console did before this, and there was no error anywhere, because
// the connection was perfectly healthy and pointed at the wrong stack.
//
// # Why it is a lookup rather than a field
//
// Same argument as orgStore (internal/console/orgscope.go): a handler that has
// not said which organisation it is acting for cannot obtain a channel at all.
// The organisation comes from the session, which came from an assertion Cloud
// signed, so it is not a value a request can choose for itself.
//
// # What it does NOT do
//
// It does not make a mixed-up lookup safe. Ask for organisation B's client
// while serving organisation A and you will get a working channel to B — the
// pool cannot know that was wrong. What the per-organisation CA buys is that a
// *mismatched pair* fails: A's credentials against B's endpoint is refused
// inside the TLS handshake, before any atlantis code runs. The pool's job is to
// make the pair impossible to mismatch by construction, since both halves come
// from one row.
type orgClients struct {
	db  *store
	now func() time.Time

	mu sync.RWMutex
	by map[string]*orgClientEntry
}

type orgClientEntry struct {
	client *adminClient

	// endpoint and health are carried so handlers that need the address —
	// the Health page reaches atlantis over plain HTTP, not gRPC — do not have
	// to re-read the row.
	endpoint string
	health   string

	// updatedAt is the row's value when this client was built. A rotation
	// moves it, and the refresh below rebuilds rather than serving a channel
	// whose certificate has been replaced.
	updatedAt time.Time

	// checkedAt is when the row was last re-read, which is not the same thing
	// and must not be conflated: one tracks the credential, the other tracks
	// our knowledge of it.
	checkedAt time.Time
}

func newOrgClients(db *store) *orgClients {
	return &orgClients{db: db, now: time.Now, by: map[string]*orgClientEntry{}}
}

// get returns the channel for an organisation, dialling it if necessary.
//
// Fails closed and by name. There is deliberately no fallback endpoint: an
// organisation nobody has registered is refused, because falling back to a
// shared address is precisely the silent cross-organisation read this whole
// step exists to prevent. One missing row would otherwise route an
// unprovisioned organisation into somebody else's atlantis, and every page
// would render.
func (p *orgClients) get(ctx context.Context, org string) (*orgClientEntry, error) {
	if org == "" {
		return nil, ErrNoOrg
	}

	p.mu.RLock()
	e, ok := p.by[org]
	p.mu.RUnlock()

	if ok && p.now().Sub(e.checkedAt) < orgClientRefresh {
		return e, nil
	}

	creds, err := p.db.orgCredentials(ctx, org)
	if err != nil {
		// An answer and a failure to answer are different things, and the
		// difference decides whether the cached client survives.
		//
		// ErrNotFound and ErrOrgNotProvisioned are answers: the database was
		// reached and said this organisation has no atlantis. That is what
		// de-provisioning looks like, and it has to take effect — a cached
		// client that outlived the row would keep serving a removed
		// organisation until the process restarted, which is the opposite of
		// what orgClientRefresh claims to bound.
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrOrgNotProvisioned) {
			p.evict(org)
			return nil, err
		}

		// Everything else is a failure to answer, and a cached client outlives
		// it. The database may be briefly unavailable, and signing an
		// organisation out of its own atlantis over a blip would turn a
		// hiccup into an outage. Same rule as the JWKS cache next door, for
		// the same reason.
		//
		// Note what this does NOT extend to: an organisation with no cached
		// client still gets the error. Failing open on first use would mean
		// dialling nothing at all, which cannot be right.
		if ok {
			return e, nil
		}
		return nil, err
	}

	// The row is unchanged, so the existing channel is still built from the
	// current credentials. Extend rather than redial — a new connection per
	// refresh would drop in-flight streams every five minutes.
	if ok && e.updatedAt.Equal(creds.UpdatedAt) {
		p.mu.Lock()
		e.checkedAt = p.now()
		p.mu.Unlock()
		return e, nil
	}

	client, err := dialOrg(creds)
	if err != nil {
		return nil, fmt.Errorf("connect to %s's atlantis at %s: %w", org, creds.Endpoint, err)
	}

	fresh := &orgClientEntry{
		client:    client,
		endpoint:  creds.Endpoint,
		health:    creds.HealthAddr,
		updatedAt: creds.UpdatedAt,
		checkedAt: p.now(),
	}

	p.mu.Lock()
	previous := p.by[org]
	p.by[org] = fresh
	p.mu.Unlock()

	// Close the replaced channel OUTSIDE the lock.
	//
	// grpc.ClientConn.Close() can block, and holding the write lock across it
	// would stall every other organisation's lookup behind one slow teardown.
	// The sandbox layer makes the same point about Unregister
	// (internal/console/sandbox.go:185).
	if previous != nil && previous.client != nil {
		_ = previous.client.Close()
	}
	return fresh, nil
}

// evict drops an organisation's channel and closes it.
//
// Closing outside the lock, for the reason given at the end of get: a blocking
// teardown must not stall every other organisation's lookup.
func (p *orgClients) evict(org string) {
	p.mu.Lock()
	e := p.by[org]
	delete(p.by, org)
	p.mu.Unlock()

	if e != nil && e.client != nil {
		_ = e.client.Close()
	}
}

// close tears down every channel.
//
// Called from Server.Close. Snapshots under the lock and closes outside it, for
// the reason above — and because Close is called during shutdown, when a
// blocked teardown is most likely.
func (p *orgClients) close() {
	p.mu.Lock()
	entries := make([]*orgClientEntry, 0, len(p.by))
	for _, e := range p.by {
		entries = append(entries, e)
	}
	p.by = map[string]*orgClientEntry{}
	p.mu.Unlock()

	for _, e := range entries {
		if e.client != nil {
			_ = e.client.Close()
		}
	}
}

// atlFor returns the admin channel for an organisation.
//
// `s.atl` used to be a field, which meant no call site could be wrong about
// which atlantis it was talking to — and also meant they were all wrong
// together, because there was only one.
func (s *Server) atlFor(ctx context.Context, org string) (*adminClient, error) {
	e, err := s.orgs.get(ctx, org)
	if err != nil {
		return nil, err
	}
	return e.client, nil
}

// orgAddrs resolves the calling session's atlantis addresses, or writes the
// failure and returns nil.
//
// Separate from orgATL because two surfaces need an address rather than a
// channel: the Settings page shows the gRPC endpoint, and the Health page
// reaches atlantis over **plain HTTP** on a different port entirely. That
// second one is the easy one to forget — it never touches the gRPC client, so
// nothing about moving the client per organisation would have flagged it, and
// an unmoved health address would have every organisation's Health page
// reporting one server's status.
func (s *Server) orgAddrs(w http.ResponseWriter, r *http.Request) *orgClientEntry {
	u, ok := r.Context().Value(ctxUser).(*User)
	if !ok {
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return nil
	}
	e, err := s.orgs.get(r.Context(), u.Org)
	if err != nil {
		s.log.Error("resolve organisation's atlantis", "org", u.Org, "err", err)
		jsonError(w, fmt.Sprintf("no atlantis is registered for %q", u.Org),
			http.StatusServiceUnavailable)
		return nil
	}
	return e
}

// orgATL resolves the calling session's atlantis, or writes the failure and
// returns nil.
//
// Every handler that talks to atlantis starts with this, and the two-line
// `if atl == nil { return }` is the price of the organisation no longer being
// implicit. It reads the organisation off the request context rather than
// taking it as an argument, so a handler cannot pass the wrong one — the same
// reasoning as orgStore, one layer out.
func (s *Server) orgATL(w http.ResponseWriter, r *http.Request) *adminClient {
	u, ok := r.Context().Value(ctxUser).(*User)
	if !ok {
		// Only reachable if a route were mounted outside s.auth. Answering
		// rather than panicking, because the alternative is a 500 that says
		// nothing.
		jsonError(w, "unauthenticated", http.StatusUnauthorized)
		return nil
	}

	atl, err := s.atlFor(r.Context(), u.Org)
	switch {
	case err == nil:
		return atl

	case errors.Is(err, ErrOrgNotProvisioned), errors.Is(err, ErrNotFound):
		// The organisation is real — somebody signed in to it — but nothing has
		// been registered for it. That is an operator's problem, not the
		// user's, so it is a 503 and not a 404, and it says which organisation
		// so the operator does not have to guess.
		s.log.Error("organisation has no atlantis registered", "org", u.Org)
		jsonError(w, fmt.Sprintf("no atlantis is registered for %q; "+
			"an operator must register it before this console can serve it", u.Org),
			http.StatusServiceUnavailable)
		return nil

	default:
		s.log.Error("resolve organisation's atlantis", "org", u.Org, "err", err)
		jsonError(w, "cannot reach this organisation's atlantis", http.StatusServiceUnavailable)
		return nil
	}
}
