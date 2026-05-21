package runtime

import (
	"strings"
	"testing"
	"time"
)

// CompositeID's stability matters more than its readable shape: cache keys
// derived from it are stored in memcached for the row's lifetime. Reordering
// arguments or changing the separator would silently invalidate every cached
// entry on next deploy.
//
// These tests pin the encoding so a "small tweak" to EncodeKeyArg or the
// joining behavior shows up as a loud failure rather than a quiet cache
// stampede.

func TestCompositeID_SingleArgMatchesEncodeKeyArg(t *testing.T) {
	// Single-PK callers should see the same string they'd get from
	// EncodeKeyArg directly — that's the whole point of the special case.
	if got, want := CompositeID(42), EncodeKeyArg(42); got != want {
		t.Errorf("CompositeID(42) = %q, want %q (EncodeKeyArg)", got, want)
	}
	if got, want := CompositeID("ab"), EncodeKeyArg("ab"); got != want {
		t.Errorf("CompositeID(\"ab\") = %q, want %q (EncodeKeyArg)", got, want)
	}
}

func TestCompositeID_MultipleArgs(t *testing.T) {
	got := CompositeID(int64(7), "ab")
	// Format: <len>:<value>|<len>:<value>
	want := "1:7|2:ab"
	if got != want {
		t.Errorf("CompositeID(7, \"ab\") = %q, want %q", got, want)
	}
}

func TestCompositeID_BarSeparatorDoesNotCollide(t *testing.T) {
	// The length prefix is what prevents collisions when values themselves
	// contain the separator. Without it, ("ab|cd", "ef") and ("ab", "cd|ef")
	// would produce the same string.
	a := CompositeID("ab|cd", "ef")
	b := CompositeID("ab", "cd|ef")
	if a == b {
		t.Errorf("CompositeID encoding collides on |-bearing values: both produced %q", a)
	}
	if !strings.HasPrefix(a, "5:") {
		t.Errorf("expected length prefix 5 for %q, got %q", "ab|cd", a)
	}
}

func TestCompositeID_TimeUsesUTC(t *testing.T) {
	// time.Time arguments collapse to RFC3339Nano in UTC so the encoding is
	// timezone-invariant; otherwise two pods in different regions could
	// build different cache keys for the same row.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("LA tz not available on this system")
	}
	ts := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	tsLA := ts.In(loc)
	if CompositeID(ts) != CompositeID(tsLA) {
		t.Errorf("CompositeID is not timezone-invariant for the same instant")
	}
}
