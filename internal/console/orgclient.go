package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
// The console serves many organisations, each with its own atlantis behind its
// own CA. A single shared client would reach one of them and return its data
// for every organisation, with no error, because the connection is healthy and
// points at the wrong stack.
//
// A lookup rather than a field, as with orgStore in orgscope.go: a handler that
// has not named an organisation cannot obtain a channel. The organisation comes
// from the session, which came from an assertion Cloud signed, so a request
// cannot choose it.
//
// This does not make a mixed-up lookup safe: asking for organisation B's client
// while serving A returns a working channel to B. What the per-organisation CA
// gives is that a mismatched pair fails — A's credentials against B's endpoint
// are refused in the TLS handshake — and both halves of the pair come from one
// row.
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

	// signer is this organisation's own certificate signer, or nil when it has
	// none and the console's process-wide client applies.
	//
	// Cached here rather than built per request for the same reason the admin
	// channel is: it is a certificate parse and a TLS config, and it is built
	// from the same row. Keeping the two together also means a rotation moves
	// both at once — updatedAt governs this exactly as it governs the channel,
	// so a signer certificate replaced in the database comes into use without a
	// restart and without a second freshness rule to get wrong.
	signer     *http.Client
	signerAddr string

	// healthClient reads the health listener, which now terminates TLS and
	// demands a client certificate on /status and /metrics. Built from the same
	// credentials as the admin channel, so one rotation moves both.
	healthClient *http.Client

	// updatedAt is the row's value when this client was built. A rotation
	// moves it, and the refresh below rebuilds rather than serving a channel
	// whose certificate has been replaced.
	updatedAt time.Time

	// checkedAt is when the row was last re-read, which is a different fact:
	// one tracks the credential, the other tracks how current this cache is.
	checkedAt time.Time
}

func newOrgClients(db *store) *orgClients {
	return &orgClients{db: db, now: time.Now, by: map[string]*orgClientEntry{}}
}

// closeEntry tears down every client an entry holds.
//
// One place rather than three, because the signer arrived after the channel and
// the three existing teardown paths would each have had to remember it. An
// *http.Client has no Close, so its transport's idle connections are what there
// is to release, and leaking those is not fatal, which is why it goes
// unnoticed. Every handle an entry acquires is released here: the health client
// arrived when the health listener began demanding a certificate, and this was
// the only place that changed.
func closeEntry(e *orgClientEntry) {
	if e == nil {
		return
	}
	if e.client != nil {
		_ = e.client.Close()
	}
	if e.signer != nil {
		e.signer.CloseIdleConnections()
	}
	if e.healthClient != nil {
		e.healthClient.CloseIdleConnections()
	}
}

// get returns the channel for an organisation, dialling it if necessary.
//
// Fails closed and by name. There is no fallback endpoint: an organisation
// nobody has registered is refused, since falling back to a shared address
// routes an unprovisioned organisation into somebody else's atlantis on one
// missing row, with every page rendering.
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
		// It does not extend to an organisation with no cached client, which
		// still gets the error: failing open on first use would mean dialling
		// nothing at all.
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

	// Built here, so a broken signer certificate is reported against the
	// organisation it belongs to rather than surfacing later as a failed
	// enrolment. Nil when the organisation has none — the caller falls back to
	// the process-wide client, which is what every organisation registered
	// before migration 0009 uses.
	var signer *http.Client
	if creds.SignerConfigured() {
		signer, err = orgSignerClient(creds)
		if err != nil {
			// The atlantis channel is already built and would be dropped on the
			// floor by returning here, so close it.
			_ = client.Close()
			return nil, fmt.Errorf("build %s's signer client: %w", org, err)
		}
	}

	healthClient, err := orgHealthClient(creds)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("build %s's health client: %w", org, err)
	}

	fresh := &orgClientEntry{
		client:       client,
		healthClient: healthClient,
		endpoint:     creds.Endpoint,
		health:       creds.HealthAddr,
		signer:       signer,
		signerAddr:   creds.SignerAddr,
		updatedAt:    creds.UpdatedAt,
		checkedAt:    p.now(),
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
	closeEntry(previous)
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

	closeEntry(e)
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
		closeEntry(e)
	}
}

// atlFor returns the admin channel for an organisation.
//
// A single `s.atl` field leaves no call site able to name the wrong atlantis,
// and every call site talking to the same one whichever organisation it is
// serving.
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
// reaches atlantis over plain HTTP on a different port. The health address
// never touches the gRPC client, so a per-organisation client alone leaves
// every organisation's Health page reporting one server's status.
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

// healthTimeout bounds one scrape of an organisation's health listener.
//
// Short. The Health page makes four of these in sequence, and a page that hangs
// for a minute because one organisation is wedged is worse than a page that
// reports the organisation as unreachable.
const healthTimeout = 5 * time.Second

// orgHealthClient builds the client that reads an organisation's health
// listener.
//
// /status and /metrics on that listener require a client certificate, which
// this presents. A NetworkPolicy is not a portable substitute: `ipBlock` covers
// pod traffic under Calico, does not under GKE Dataplane V2, and excludes
// nothing on EKS.
//
// It presents the organisation's atlantis credentials, not its signer
// credentials — the same authority the admin channel authenticates against, so
// one rotation moves both. orgSignerClient is the same shape against the other
// root; they are separate functions because the roots must stay separate.
func orgHealthClient(creds *orgCredentials) (*http.Client, error) {
	cert, err := tls.X509KeyPair([]byte(creds.CertPEM), creds.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("client certificate for %s does not match its key: %w",
			creds.Org, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(creds.CAPEM)) {
		return nil, fmt.Errorf("CA for %s contains no usable certificates", creds.Org)
	}
	return &http.Client{
		Timeout: healthTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
			},
		},
	}, nil
}
