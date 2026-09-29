package console

// The parts of a sweep that need no database and no tenant.
//
// Two of them are the sweep's own arithmetic rather than something a server
// computes: whether a freeze window covers now, and what kind of failure an
// unreachable organisation had. Both decide what an operator is told, and
// neither is visible in any other test.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

func window(start, end time.Time) *adminpb.FreezeWindow {
	return &adminpb.FreezeWindow{
		StartsAt: start.Format(time.RFC3339),
		EndsAt:   end.Format(time.RFC3339),
	}
}

// A freeze window is open only while it covers the moment asked about.
//
// ListFreezeWindows returns every row it has, expired ones included, and has no
// notion of the current time. Reporting a window that ended last month as an
// open freeze would put every organisation that has ever frozen into the state
// operators are meant to act on.
func TestAFreezeWindowIsOpenOnlyWhileItCoversNow(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	hour := time.Hour

	cases := []struct {
		what    string
		windows []*adminpb.FreezeWindow
		open    bool
	}{
		{"none at all", nil, false},
		{"one that ended an hour ago",
			[]*adminpb.FreezeWindow{window(now.Add(-2*hour), now.Add(-hour))}, false},
		{"one that starts in an hour",
			[]*adminpb.FreezeWindow{window(now.Add(hour), now.Add(2*hour))}, false},
		{"one covering now",
			[]*adminpb.FreezeWindow{window(now.Add(-hour), now.Add(hour))}, true},
		{"one starting exactly now",
			[]*adminpb.FreezeWindow{window(now, now.Add(hour))}, true},
		// The end is exclusive, so a window ending now has lifted.
		{"one ending exactly now",
			[]*adminpb.FreezeWindow{window(now.Add(-hour), now)}, false},
		{"an expired one beside an open one",
			[]*adminpb.FreezeWindow{
				window(now.Add(-5*hour), now.Add(-4*hour)),
				window(now.Add(-hour), now.Add(hour)),
			}, true},
		{"one whose timestamps do not parse",
			[]*adminpb.FreezeWindow{{StartsAt: "yesterday", EndsAt: "tomorrow"}}, false},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			open, _ := freezeOpenAt(c.windows, now)
			if open != c.open {
				t.Errorf("open = %v, want %v", open, c.open)
			}
		})
	}
}

// The end reported is the latest among the windows covering now.
//
// Two overlapping freezes lift when the later one does. Reporting the earlier
// end would tell an operator the freeze had gone while it had not.
func TestTheFreezeEndIsTheLatestOfTheCoveringWindows(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	later := now.Add(4 * time.Hour)

	open, endsAt := freezeOpenAt([]*adminpb.FreezeWindow{
		window(now.Add(-time.Hour), now.Add(time.Hour)),
		window(now.Add(-time.Hour), later),
		// Expired, and later than both. Not covering now, so not counted.
		window(now.Add(-9*time.Hour), now.Add(-8*time.Hour)),
	}, now)

	if !open {
		t.Fatal("two windows cover now and the freeze was reported closed")
	}
	if !endsAt.Equal(later) {
		t.Errorf("ends at %s, want the later of the covering windows, %s", endsAt, later)
	}
}

// Parked objects past their retention are counted apart from the rest.
//
// The total says how much is still recoverable; this says how much should
// already have gone, which is the question the register is read for.
func TestOnlyUnreapedObjectsPastRetentionAreOverdue(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }

	objects := []*adminpb.ParkedObject{
		{ReapAfter: rfc(now.Add(-time.Hour))},
		{ReapAfter: rfc(now.Add(time.Hour))},
		// Already reaped, so it is not waiting for anything.
		{ReapAfter: rfc(now.Add(-time.Hour)), ReapedAt: rfc(now.Add(-time.Minute))},
		{ReapAfter: "not a timestamp"},
	}

	if got := countOverdue(objects, now); got != 1 {
		t.Errorf("overdue = %d, want 1 — one unreaped object is past its reap_after", got)
	}
}

// An unreachable organisation is classified by cause.
//
// Every other failure path in this package becomes a 502 carrying the raw
// string, under which a pod that is down, a certificate that expired and a
// tenant refusing this console's credential read the same. They call for
// different work.
func TestClassifyTellsAPodDownFromACertificateThatExpired(t *testing.T) {
	cases := []struct {
		what string
		err  error
		kind string
	}{
		{"no registry row", fmt.Errorf("read: %w", ErrNotFound), kindUnregistered},
		{"registered but not provisioned",
			fmt.Errorf("resolve: %w", ErrOrgNotProvisioned), kindUnprovisioned},
		{"the sweep's own deadline", context.DeadlineExceeded, kindTimeout},
		{"a socket deadline", os.ErrDeadlineExceeded, kindTimeout},
		{"a certificate that does not verify",
			&net.OpError{Op: "remote error", Err: &tls.CertificateVerificationError{}}, kindTLS},
		// The shape a peer rejecting this console's client certificate takes:
		// a TLS alert inside an OpError. Not a CertificateVerificationError, so
		// the case above does not cover it, and an OpError check reached first
		// reports it as a pod that is down.
		{"the tenant rejecting this console's certificate",
			&net.OpError{Op: "remote error", Err: errors.New("tls: bad certificate")}, kindTLS},
		{"a handshake the tenant refused",
			&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, kindTLS},
		{"a certificate the gRPC transport folded into Unavailable",
			status.Error(codes.Unavailable, "connection error: x509: certificate has expired"), kindTLS},
		// Stopping the console cancels the sweep. Not a fact about the tenant.
		{"the sweep stopped, over HTTP",
			&url.Error{Op: "Get", URL: "https://tenant/status", Err: context.Canceled}, kindCancelled},
		{"the sweep stopped, over gRPC",
			status.Error(codes.Canceled, "context canceled"), kindCancelled},
		{"a name that does not resolve",
			&net.DNSError{Name: "atlantis.org-gone"}, kindDNS},
		{"a refused connection",
			&net.OpError{Op: "dial", Err: errors.New("connection refused")}, kindDial},
		{"a tenant refusing this console",
			status.Error(codes.PermissionDenied, "caller not registered"), kindRefused},
		{"a tenant that is not answering",
			status.Error(codes.Unavailable, "connection refused"), kindDial},
		{"anything else the tenant said",
			status.Error(codes.Internal, "boom"), kindRPC},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			kind, text := classifyUnreachable(c.err)
			if kind != c.kind {
				t.Errorf("kind = %q, want %q", kind, c.kind)
			}
			if text == "" {
				t.Error("no text was kept, so the row records a failure with no detail")
			}
		})
	}
}

// A tenant older than an RPC is not an unreachable tenant.
//
// Three organisations were once found running three different builds at once,
// so a tenant that has never heard of a call is a live state. Treating it as
// unreachable would report an outage on an organisation that is serving.
func TestATenantThatDoesNotKnowAnRPCIsStillReachable(t *testing.T) {
	err := status.Error(codes.Unimplemented, "unknown method ListParkedObjects")
	if kind, _ := classifyUnreachable(err); kind != kindRPC {
		t.Errorf("kind = %q, want %q — Unimplemented is one fact missing, not an outage",
			kind, kindRPC)
	}
}

// The recorded error text is bounded.
//
// A TLS failure carries the certificate chain it rejected, which runs to
// kilobytes. One row should not be a page of text.
func TestARecordedErrorIsTruncated(t *testing.T) {
	long := make([]byte, maxFactError*3)
	for i := range long {
		long[i] = 'x'
	}
	_, text := classifyUnreachable(errors.New(string(long)))
	if len(text) != maxFactError {
		t.Errorf("kept %d bytes, want %d", len(text), maxFactError)
	}
}

// The metrics listener answers Prometheus text on its own address.
func TestTheMetricsListenerServesPrometheusText(t *testing.T) {
	srv := newMetricsServer("127.0.0.1:0")
	lis, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srv.Close() }()
	go func() { _ = srv.Serve(lis) }()

	resp, err := http.Get("http://" + lis.Addr().String() + "/metrics") //nolint:noctx // a test against a listener it just started
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
}

// A sweep's budget is a share of its own interval.
//
// Asserted in real units because the arithmetic is the kind that goes wrong
// silently: computing the share as a Duration and dividing by time.Second
// multiplies two Durations, which for five minutes overflows int64 and wraps to
// 192ms. A sweep so bounded ends before it has visited anything, and on a
// loopback fixture it still finishes in time to look correct.
func TestASweepIsBoundedByAShareOfItsInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{5 * time.Minute, 4 * time.Minute},
		{time.Minute, 48 * time.Second},
		{10 * time.Second, 8 * time.Second},
		// Unset, so the default interval applies.
		{0, 4 * time.Minute},
	}
	for _, c := range cases {
		s := &Server{cfg: Config{FleetPollInterval: c.interval}}
		if got := s.fleetSweepTimeout(); got != c.want {
			t.Errorf("interval %s gives a sweep budget of %s, want %s", c.interval, got, c.want)
		}
	}
}

// A sweep's budget never exceeds the gap to the next one.
func TestASweepBudgetFitsInsideItsInterval(t *testing.T) {
	for _, interval := range []time.Duration{
		time.Second, 30 * time.Second, 5 * time.Minute, time.Hour,
	} {
		s := &Server{cfg: Config{FleetPollInterval: interval}}
		budget := s.fleetSweepTimeout()
		if budget <= 0 {
			t.Errorf("interval %s gives a budget of %s, which starts expired", interval, budget)
		}
		if budget >= interval {
			t.Errorf("interval %s gives a budget of %s, so a sweep can outlive its tick",
				interval, budget)
		}
	}
}
