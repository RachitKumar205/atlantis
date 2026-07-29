package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// fakeDB returns a fixed set of capability names per caller and counts reads,
// so the cache's behaviour is observable without Postgres.
type fakeDB struct {
	byCaller map[string][]string
	err      error
	queries  int
}

func (f *fakeDB) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	f.queries++
	if f.err != nil {
		return nil, f.err
	}
	caller, _ := args[0].(string)
	return &fakeRows{vals: f.byCaller[caller]}, nil
}

type fakeRows struct {
	vals []string
	i    int
}

func (r *fakeRows) Next() bool { r.i++; return r.i <= len(r.vals) }
func (r *fakeRows) Scan(dest ...any) error {
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("unexpected scan target")
	}
	*p = r.vals[r.i-1]
	return nil
}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Close()     {}

func newGrants(t *testing.T, db Querier, caller string) Grants {
	t.Helper()
	g, err := NewPostgresGrants(PostgresGrantsConfig{
		DB:                db,
		CallerFromContext: func(context.Context) string { return caller },
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPostgresGrantsResolvesCapabilities(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{
		"ci": {"CAPABILITY_SCHEMA_READ", "CAPABILITY_SCHEMA_APPLY"},
	}}
	got, err := newGrants(t, db, "ci").For(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Has(adminpb.Capability_CAPABILITY_SCHEMA_APPLY) {
		t.Error("SCHEMA_APPLY missing")
	}
	if got.Has(adminpb.Capability_CAPABILITY_OPERATOR) {
		t.Error("OPERATOR must not appear from nowhere")
	}
}

// An unrecognized grant must confer nothing. An older binary reading a
// capability a newer one added has to fail closed, and a typo must not be
// indistinguishable from a valid grant.
func TestUnknownCapabilityIsIgnored(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{
		"ci": {"CAPABILITY_SCHEMA_READ", "CAPABILITY_TIME_TRAVEL", "not_a_capability", ""},
	}}
	got, err := newGrants(t, db, "ci").For(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got.Has(adminpb.Capability_CAPABILITY_SCHEMA_READ) {
		t.Errorf("expected only SCHEMA_READ, got %v", got)
	}
}

// CAPABILITY_UNSPECIFIED is a real enum name, so Capability_value resolves it.
// It must still be dropped: the zero value means "declared nothing", and a row
// carrying it would otherwise satisfy a method whose requirement failed to
// parse.
func TestUnspecifiedGrantIsDropped(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{"ci": {"CAPABILITY_UNSPECIFIED"}}}
	got, err := newGrants(t, db, "ci").For(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("UNSPECIFIED must confer nothing, got %v", got)
	}
}

// An unauthenticated connection resolves to no caller. That is an empty grant
// set, not a lookup error — the interceptor denies either way, but the error
// path would point whoever is debugging at the database instead of at the
// missing client certificate.
func TestAnonymousCallerGetsEmptySetWithoutQuerying(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{}}
	got, err := newGrants(t, db, "").For(context.Background())
	if err != nil {
		t.Fatalf("anonymous should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty set, got %v", got)
	}
	if db.queries != 0 {
		t.Errorf("anonymous should not hit the database, got %d queries", db.queries)
	}
}

func TestGrantsAreCachedWithinTTL(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{"ci": {"CAPABILITY_SCHEMA_READ"}}}
	g := newGrants(t, db, "ci")
	for i := 0; i < 5; i++ {
		if _, err := g.For(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if db.queries != 1 {
		t.Errorf("expected 1 query within the TTL window, got %d", db.queries)
	}
}

func TestCacheExpires(t *testing.T) {
	db := &fakeDB{byCaller: map[string][]string{"ci": {"CAPABILITY_SCHEMA_READ"}}}
	g, err := NewPostgresGrants(PostgresGrantsConfig{
		DB:                db,
		CallerFromContext: func(context.Context) string { return "ci" },
		CacheTTL:          time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.For(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := g.For(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.queries != 2 {
		t.Errorf("expected a refresh after the TTL, got %d queries", db.queries)
	}
}

// A failed lookup must not be cached: caching a denial would stretch a
// transient database blip into a TTL-long outage for that caller.
func TestLookupErrorIsNotCached(t *testing.T) {
	db := &fakeDB{err: errors.New("connection refused")}
	g := newGrants(t, db, "ci")

	if _, err := g.For(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := g.For(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if db.queries != 2 {
		t.Errorf("errors must not be cached, got %d queries", db.queries)
	}
}

func TestNewPostgresGrantsRequiresDependencies(t *testing.T) {
	if _, err := NewPostgresGrants(PostgresGrantsConfig{
		CallerFromContext: func(context.Context) string { return "" },
	}); err == nil {
		t.Error("expected an error without a DB")
	}
	if _, err := NewPostgresGrants(PostgresGrantsConfig{DB: &fakeDB{}}); err == nil {
		t.Error("expected an error without CallerFromContext")
	}
}
