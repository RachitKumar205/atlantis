package entity

import (
	"fmt"
	"sync/atomic"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/cache/read"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Server is the dynamic entity server that reads the DSL IR at startup
// and serves every entity's CRUD RPCs without compiled proto stubs.
// Handlers load metadata from the current snapshot at each request,
// enabling hot-reload via atomic pointer swap.
type Server struct {
	pool       runtime.Pool
	cache      runtime.Cache
	outbox     runtime.Outbox
	queryCache *queryresult.Cache
	// onReload verifies a new IR before it becomes live. See Reload.
	onReload func(*dsl.IR) error
	// reader is the read-through cache path. Nil disables caching entirely,
	// which is what the sandbox and most tests want.
	//
	// It is a constructor argument rather than a settable field, but that is
	// not what keeps it wired: queryCache has been a required argument since
	// this server was written and was never once read. Only a test that
	// asserts a second Get does not reach Postgres can keep this honest —
	// see TestGetIsServedFromCacheOnSecondCall.
	reader   *read.Reader
	snapshot atomic.Pointer[entitySnapshot]
}

// NewServer constructs a dynamic entity server.
func NewServer(pool runtime.Pool, cache runtime.Cache, outbox runtime.Outbox, qc *queryresult.Cache, reader *read.Reader) *Server {
	s := &Server{
		pool:       pool,
		cache:      cache,
		outbox:     outbox,
		queryCache: qc,
		reader:     reader,
	}
	s.snapshot.Store(&entitySnapshot{
		entities:   make(map[string]*entityMeta),
		customMeta: make(map[string]*customQueryMeta),
		procMeta:   make(map[string]*customProcMeta),
	})
	return s
}

// Reload builds a new snapshot from the IR and swaps it atomically.
// In-flight requests on the old snapshot complete unaffected.
//
// The OnReload hook runs first and can refuse: a schema that would turn on
// tenant isolation the database is not enforcing must not become live just
// because it arrived after boot.
func (s *Server) Reload(ir *dsl.IR, contentHash string) error {
	snap, err := buildSnapshot(ir, contentHash)
	if err != nil {
		return fmt.Errorf("entity.Reload: %w", err)
	}
	if s.onReload != nil {
		if err := s.onReload(ir); err != nil {
			return fmt.Errorf("entity.Reload: refusing the new schema: %w", err)
		}
	}
	return s.install(snap)
}

// SetOnReload installs a hook called with the new IR before the snapshot is
// swapped in. A non-nil error abandons the reload and leaves the old snapshot
// serving. Not a constructor argument, because most callers (sandbox, tests)
// have nothing to verify.
//
// It carries the partition-policy check, which asks the database whether every
// entity declaring `partition by` has an enforced policy on its table. No
// differ can answer that: a differ compares two declarations, and this compares
// a declaration against the live catalogue. A policy goes missing from a
// correct declaration in ways that are not diffs — an unapplied migration, an
// adopted database, a policy dropped out of band.
//
// Checking at boot is not enough. A checkpoint reaches a running server through
// LISTEN/NOTIFY, and the reload turns on `partitioned` for whatever the table
// carries at that moment, while the one observable signal — omit the tenant,
// get refused — reports healthy throughout.
//
// A func rather than a direct call, because this package must not import
// internal/storage/pg: pg already depends on runtime, and the check belongs
// next to the other catalog probes.
func (s *Server) SetOnReload(fn func(*dsl.IR) error) { s.onReload = fn }

// ContentHash returns the content hash of the currently loaded snapshot.
func (s *Server) ContentHash() string {
	snap := s.snapshot.Load()
	if snap == nil {
		return ""
	}
	return snap.contentHash
}
