package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The cap counts the last 24 hours only, and the call that would pass it
// stores nothing.
func TestEnquiryDailyCap(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	const old = 10
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO cloud.enquiries (email, message, created_at)
		SELECT 'old@example.com', 'yesterday', now() - interval '25 hours' FROM generate_series(1, $1::int)`,
		old); err != nil {
		t.Fatalf("seed old enquiries: %v", err)
	}
	if _, err := db.pool.Exec(ctx, `
		INSERT INTO cloud.enquiries (email, message)
		SELECT 'ada@example.com', 'today' FROM generate_series(1, $1::int)`,
		EnquiryDailyCap-1); err != nil {
		t.Fatalf("seed today's enquiries: %v", err)
	}

	if _, err := db.CreateEnquiry(ctx, "ada@example.com", "the last one"); err != nil {
		t.Fatalf("enquiry %d of %d: %v", EnquiryDailyCap, EnquiryDailyCap, err)
	}
	if _, err := db.CreateEnquiry(ctx, "ada@example.com", "one too many"); !errors.Is(err, ErrEnquiryCap) {
		t.Fatalf("enquiry %d: err = %v, want ErrEnquiryCap", EnquiryDailyCap+1, err)
	}

	var n int
	if err := db.pool.QueryRow(ctx, `SELECT count(*) FROM cloud.enquiries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != EnquiryDailyCap+old {
		t.Fatalf("stored %d enquiries, want %d", n, EnquiryDailyCap+old)
	}
}

// Migration 0016's CHECKs refuse what CreateEnquiry's callers are meant to
// have refused already, so a caller that skips the checks is stopped here.
func TestEnquiryChecksRefuseWhatTheHandlerRefuses(t *testing.T) {
	db := newTestStore(t)
	ctx := context.Background()

	cases := []struct{ name, email, message string }{
		{"unfolded email", "Ada@Example.com", "hello"},
		{"padded email", " ada@example.com", "hello"},
		{"blank message", "ada@example.com", "   "},
		{"long message", "ada@example.com", strings.Repeat("a", EnquiryMaxLen+1)},
	}
	for _, c := range cases {
		if _, err := db.pool.Exec(ctx,
			`INSERT INTO cloud.enquiries (email, message) VALUES ($1, $2)`, c.email, c.message); err == nil {
			t.Errorf("%s: the row was accepted", c.name)
		}
	}

	id, err := db.CreateEnquiry(ctx, " Ada@Example.com ", strings.Repeat("a", EnquiryMaxLen))
	if err != nil {
		t.Fatalf("CreateEnquiry folds the address and accepts the limit: %v", err)
	}
	rows, err := db.Enquiries(ctx)
	if err != nil {
		t.Fatalf("Enquiries: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].Email != "ada@example.com" {
		t.Fatalf("Enquiries = %+v, want the one row, folded", rows)
	}
}
