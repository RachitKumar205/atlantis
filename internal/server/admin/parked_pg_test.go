package admin

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// ListParkedObjects is tested against a real database because the whole RPC is
// one query. The interesting parts are all SQL: the include_reaped filter, the
// ordering that puts the soonest-to-expire first, and the nullable timestamps —
// reaped_at and next_attempt_after are NULL for most rows, and scanning a NULL
// into a non-pointer is a runtime error that no compile-time check catches.
//
//	ATLANTIS_TEST_PG=postgres://atlantis:atlantis@localhost:5432/atlantis?sslmode=disable \
//	  go test ./internal/server/admin/ -run ListParked -v
func TestListParkedObjects(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the parked-objects test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	clean := func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'lpo_%'`)
	}
	clean()
	t.Cleanup(clean)

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.parked_objects
  (kind, schema_name, object_name, parent_table, original_schema, original_name,
   reap_after, reaped_at, attempts, last_error) VALUES
  ('table','atlantis_tombstone','lpo_far__parked',   NULL,   'shop','lpo_far',   now() + INTERVAL '29 days', NULL, 0, NULL),
  ('table','atlantis_tombstone','lpo_soon__parked',  NULL,   'shop','lpo_soon',  now() + INTERVAL '2 days',  NULL, 0, NULL),
  ('column','shop','lpo_col__parked','orders','shop','lpo_col', now() + INTERVAL '10 days', NULL, 3, 'depends on view v'),
  ('table','atlantis_tombstone','lpo_gone__parked',  NULL,   'shop','lpo_gone',  now() - INTERVAL '1 day', now(), 0, NULL)
`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(pool, Config{})

	// Default: live registrations only. The reaped row is an audit record, not
	// something anybody can act on, and it accumulates forever.
	resp, err := svc.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{})
	if err != nil {
		t.Fatalf("ListParkedObjects: %v", err)
	}
	var got []string
	for _, o := range resp.GetObjects() {
		if len(o.GetOriginalName()) >= 4 && o.GetOriginalName()[:4] == "lpo_" {
			got = append(got, o.GetOriginalName())
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want the 3 unreaped rows. A reaped object listed as "+
			"recoverable tells somebody their data is still there when it is not", got)
	}
	// Soonest to expire first: the list answers what is about to be lost, so
	// the ordering is part of the answer.
	if got[0] != "lpo_soon" {
		t.Errorf("first row is %q, want lpo_soon. Rows must be ordered by how "+
			"soon they stop being recoverable", got[0])
	}

	// Every scanned column asserted by VALUE, not just by presence.
	//
	// Column order in a Scan is the classic silent defect: swapping parked_at
	// and reap_after compiles, passes any ordering assertion (that comes from
	// SQL, not the scan), and makes every live object render "due now" in both
	// the CLI and the console while being perfectly recoverable. The window is
	// the entire product promise here, so it is checked directly.
	for _, o := range resp.GetObjects() {
		if o.GetOriginalName() != "lpo_soon" {
			continue
		}
		reapAfter, err := time.Parse(time.RFC3339, o.GetReapAfter())
		if err != nil {
			t.Fatalf("reap_after %q is not a timestamp: %v", o.GetReapAfter(), err)
		}
		parkedAt, err := time.Parse(time.RFC3339, o.GetParkedAt())
		if err != nil {
			t.Fatalf("parked_at %q is not a timestamp: %v", o.GetParkedAt(), err)
		}
		// Seeded at now()+2d, so it must be in the future and after parked_at.
		if !reapAfter.After(time.Now()) {
			t.Errorf("reap_after = %s, which is in the past for an object seeded "+
				"two days out. Every surface derives 'recoverable for' from this "+
				"field alone, so a wrong value tells the operator their data is "+
				"about to go when it is not", o.GetReapAfter())
		}
		if !parkedAt.Before(reapAfter) {
			t.Errorf("parked_at %s is not before reap_after %s; the two columns "+
				"are transposed", o.GetParkedAt(), o.GetReapAfter())
		}
		if o.GetSchemaName() != "atlantis_tombstone" || o.GetObjectName() != "lpo_soon__parked" {
			t.Errorf("schema_name=%q object_name=%q, want atlantis_tombstone / "+
				"lpo_soon__parked. The reaper addresses the object through both, "+
				"so transposing them reaps nothing or the wrong thing",
				o.GetSchemaName(), o.GetObjectName())
		}
	}

	// Nullable columns must survive the scan.
	for _, o := range resp.GetObjects() {
		if o.GetOriginalName() == "lpo_col" {
			if o.GetKind() != "column" || o.GetParentTable() != "orders" {
				t.Errorf("column registration lost its parent table: %+v", o)
			}
			if o.GetAttempts() != 3 || o.GetLastError() == "" {
				t.Errorf("a stuck object reports attempts=%d last_error=%q; without "+
					"both, an operator asking why it is still there has nothing to "+
					"go on", o.GetAttempts(), o.GetLastError())
			}
			if o.GetReapedAt() != "" {
				t.Errorf("an unreaped object reports reaped_at=%q", o.GetReapedAt())
			}
			if o.GetOriginalSchema() != "shop" {
				t.Errorf("original_schema = %q, want shop — a restore needs to know "+
					"where the object goes back to", o.GetOriginalSchema())
			}
		}
	}

	// include_reaped surfaces the audit trail.
	all, err := svc.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{IncludeReaped: true})
	if err != nil {
		t.Fatalf("ListParkedObjects(all): %v", err)
	}
	var reaped int
	for _, o := range all.GetObjects() {
		if o.GetOriginalName() == "lpo_gone" && o.GetReapedAt() != "" {
			reaped++
		}
	}
	if reaped != 1 {
		t.Error("include_reaped did not return the reaped row, so there is no way " +
			"to answer 'did this table exist, and when was it dropped'")
	}
}

// The limit is clamped, and truncation is reported.
//
// Reaped registrations are retained indefinitely by design, so an unbounded
// limit is a request to serialize the entire audit history into one message —
// measured at ~146 MB of heap for 300k rows, on a capability every registered
// caller holds. A list of what is still recoverable must not stop silently.
func TestListParkedObjectsClampsAndReportsTruncation(t *testing.T) {
	url := os.Getenv("ATLANTIS_TEST_PG")
	if url == "" {
		t.Skip("set ATLANTIS_TEST_PG to run the parked-objects test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	clean := func() {
		_, _ = pool.Exec(ctx,
			`DELETE FROM atlantis.parked_objects WHERE original_name LIKE 'clamp_%'`)
	}
	clean()
	t.Cleanup(clean)

	if _, err := pool.Exec(ctx, `
INSERT INTO atlantis.parked_objects
  (kind, schema_name, object_name, original_schema, original_name, reap_after)
SELECT 'table', 'atlantis_tombstone', 'clamp_' || i || '__parked', 'shop', 'clamp_' || i,
       now() + (i || ' days')::interval
  FROM generate_series(1, $1) AS i`, maxParkedLimit+20); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := New(pool, Config{})

	// A small explicit limit must truncate and report that it did.
	resp, err := svc.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{Limit: 5})
	if err != nil {
		t.Fatalf("ListParkedObjects: %v", err)
	}
	if len(resp.GetObjects()) != 5 {
		t.Errorf("got %d objects for limit=5, want 5", len(resp.GetObjects()))
	}
	if !resp.GetHasMore() {
		t.Error("has_more is false while rows were withheld. A caller cannot " +
			"distinguish 'nothing else is parked' from 'the page stops here', on " +
			"a list that exists to answer what can still be recovered")
	}

	// An absurd limit must be clamped rather than honoured. The fixture seeds
	// more rows than the ceiling: below the clamp, an unclamped server returns
	// the same answer as a clamped one and the assertion cannot fail.
	huge, err := svc.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{Limit: 2147483647})
	if err != nil {
		t.Fatalf("ListParkedObjects(huge): %v", err)
	}
	if len(huge.GetObjects()) != maxParkedLimit {
		t.Errorf("returned %d objects for limit=MaxInt32, want the clamp at %d. "+
			"Unclamped, one call serializes the whole register — reaped rows are "+
			"retained forever, so that grows without bound",
			len(huge.GetObjects()), maxParkedLimit)
	}
	if !huge.GetHasMore() {
		t.Error("a clamped response does not report has_more, so a caller cannot " +
			"tell the server withheld rows")
	}
}

// A server with no pool must not panic. Every other RPC in this package carries
// the same guard for the same reason: an empty probe request reaches the pool
// with nothing to reject on first.
func TestListParkedObjectsWithoutPool(t *testing.T) {
	var s Service
	resp, err := s.ListParkedObjects(context.Background(), &adminpb.ListParkedObjectsRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetObjects()) != 0 {
		t.Errorf("got %d objects from a pool-less service", len(resp.GetObjects()))
	}
}
