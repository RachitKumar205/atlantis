package authz

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// Querier is the subset of pgxpool this package needs, so tests can supply a
// fake without standing up Postgres.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// Rows mirrors pgx.Rows narrowly enough to iterate one text column.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// CallerFromContext extracts the authenticated caller identity a request
// arrived under. In production this is the mTLS common name resolved by the
// interceptor upstream in the chain; the same extractor is shared with the
// auth, cert-binding, and rate-limit layers so every one of them agrees on
// who is calling.
type CallerFromContext func(ctx context.Context) string

// PostgresGrantsConfig configures the database-backed grant lookup.
type PostgresGrantsConfig struct {
	DB                Querier
	CallerFromContext CallerFromContext

	// CacheTTL bounds how long a revocation takes to take effect. Defaults to
	// 5s to match the cert-binding checker — the two answer adjacent questions
	// about the same identity, and staggering their windows would only make
	// "why is this caller still allowed" harder to reason about during an
	// incident.
	CacheTTL time.Duration

	// Logger receives a warning when a stored grant names a capability this
	// binary does not recognize. Optional.
	Logger *slog.Logger
}

// NewPostgresGrants builds a Grants backed by atlantis.caller_capabilities.
func NewPostgresGrants(cfg PostgresGrantsConfig) (Grants, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("authz: PostgresGrants requires a DB")
	}
	if cfg.CallerFromContext == nil {
		return nil, fmt.Errorf("authz: PostgresGrants requires CallerFromContext")
	}
	ttl := cfg.CacheTTL
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	return &postgresGrants{
		db:     cfg.DB,
		caller: cfg.CallerFromContext,
		log:    cfg.Logger,
		cache:  &grantCache{ttl: ttl},
	}, nil
}

type postgresGrants struct {
	db     Querier
	caller CallerFromContext
	log    *slog.Logger
	cache  *grantCache
}

// For resolves the calling identity's capabilities.
//
// An unauthenticated request returns the empty set rather than an error. The
// distinction matters at the interceptor: an empty set denies every non-public
// method, while an error would also deny but would be reported as a lookup
// failure and send whoever is debugging it toward the database instead of
// toward the missing client certificate.
//
// Both spellings of "no identity" are handled. cmd/server's extractor returns
// the literal "anonymous" when no cert CN is available, and other callers
// return the empty string; treating only one would send the other through a
// pointless query for a caller that cannot exist, since "anonymous" is a
// reserved name RegisterCaller refuses.
func (g *postgresGrants) For(ctx context.Context) (Set, error) {
	caller := g.caller(ctx)
	if caller == "" || caller == "anonymous" {
		return NewSet(), nil
	}
	return g.cache.lookup(ctx, caller, g.load)
}

const grantsQuery = `SELECT capability FROM atlantis.caller_capabilities WHERE caller = $1`

// load reads one caller's grants. A row naming a capability this binary does
// not know is skipped and logged: an older server must not honor a capability
// added by a newer one, and a typo must not be silently indistinguishable
// from a correct grant that happens to confer nothing.
func (g *postgresGrants) load(ctx context.Context, caller string) (Set, error) {
	rows, err := g.db.Query(ctx, grantsQuery, caller)
	if err != nil {
		return nil, fmt.Errorf("authz: querying capabilities for %q: %w", caller, err)
	}
	defer rows.Close()

	out := NewSet()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("authz: scanning capability for %q: %w", caller, err)
		}
		value, ok := adminpb.Capability_value[name]
		if !ok || adminpb.Capability(value) == adminpb.Capability_CAPABILITY_UNSPECIFIED {
			if g.log != nil {
				g.log.Warn("authz: ignoring unrecognized capability grant",
					"caller", caller, "capability", name)
			}
			continue
		}
		out[adminpb.Capability(value)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("authz: iterating capabilities for %q: %w", caller, err)
	}
	return out, nil
}

// grantCache is a per-process TTL cache of caller → granted set, mirroring the
// cert-binding checker's cache: one read per CN per window under burst.
//
// Nothing is cached on error. A failed lookup denies the request, and caching
// that outcome would extend a transient database blip into a TTL-long outage
// for the affected caller.
type grantCache struct {
	mu  sync.RWMutex
	m   map[string]grantEntry
	ttl time.Duration
}

type grantEntry struct {
	set     Set
	expires time.Time
}

func (c *grantCache) lookup(ctx context.Context, caller string, load func(context.Context, string) (Set, error)) (Set, error) {
	now := time.Now()
	c.mu.RLock()
	e, ok := c.m[caller]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.set, nil
	}

	set, err := load(ctx, caller)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]grantEntry)
	}
	c.m[caller] = grantEntry{set: set, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return set, nil
}
