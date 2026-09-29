package console

// The fleet sweep: what every registered organisation is running, collected in
// one place.
//
// Each organisation's atlantis schema is the only record of its schema version,
// its dead-letter queue and its parked objects, and the control plane holds
// none of it — so "which organisations are behind" is N databases opened by
// hand. This visits them on a timer and writes what it saw to
// console.org_facts, with the gauges in metrics.go as the fleet view.
//
// The console is the only component that can. Each tenant's NetworkPolicy
// admits the console's namespace and pod label together, and the comment on it
// (internal/cloud/provision/objects.go) states that Cloud, the provisioner and
// a debugging shell have no business on a tenant's admin port.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

const (
	// defaultFleetPollInterval is the gap between sweeps.
	//
	// Five minutes, matching orgClientRefresh: the sweep never asks the client
	// cache to re-read a row sooner than the cache would anyway, so a rotated
	// credential comes into use on the following sweep. None of the four facts
	// moves faster in a way anybody acts on — a schema version changes on
	// apply, a freeze window is opened hours ahead, a dead-job queue
	// accumulates.
	defaultFleetPollInterval = 5 * time.Minute

	// firstFleetPollDelay keeps the first sweep clear of startup, as
	// auditRetentionLoop does. Migrations, the policy check and the first
	// sign-ins are all ahead of it.
	//
	// The tests depend on it too. Every fixture starts this poller through
	// New, and the fleet tests drive runFleetPoll themselves against the same
	// package-level gauges; no console test runs for two minutes, so the two
	// never overlap. Shortening it makes those tests race the poller.
	firstFleetPollDelay = 2 * time.Minute

	// fleetOrgTimeout bounds one organisation.
	//
	// Up to four network round trips to a stack that may be wedged. This is
	// what keeps one such organisation from consuming the sweep: it occupies
	// one worker slot for this long and nothing more.
	defaultFleetOrgTimeout = 15 * time.Second

	// fleetConcurrency is how many organisations are visited at once.
	//
	// Worst case for a sweep is ceil(N/8) × fleetOrgTimeout, which crosses four
	// minutes at about 128 organisations. Past that the interval or this number
	// has to move.
	//
	// The sweep also fills orgClients for every registered organisation, where
	// before only those someone had signed in to had an entry. Each holds a
	// gRPC connection, two HTTP transports and a parsed key pair, and nothing
	// evicts an idle one, so the console's open-file limit is the ceiling on
	// fleet size — reached first as dial failures on requests from browsers.
	fleetConcurrency = 8

	// fleetSweepNumerator and fleetSweepDenominator are the share of one
	// interval a sweep may take, so a sweep cannot outlive the tick that
	// started it. Four fifths of five minutes is four minutes.
	//
	// Untyped integers. Writing the share as a Duration and dividing by
	// time.Second multiplies two Durations: 5 minutes by 800ms is 2.4e20
	// nanoseconds, which overflows int64 and wraps to 192ms. Every sweep then
	// ends before it has visited anything, and on a loopback test fixture it
	// still finishes in time to look correct.
	fleetSweepNumerator   = 4
	fleetSweepDenominator = 5

	// fleetListLimit caps both list calls.
	//
	// ListDeadJobs honours it as a maximum and reports no total, so a response
	// of exactly this length is recorded as truncated. ListParkedObjects
	// clamps to 500 itself and answers with has_more.
	fleetListLimit = 200

	// fleetWriteTimeout bounds the whole write phase after the network work,
	// so a database that has stopped answering cannot wedge a sweep that has
	// already collected everything. Each write is one transaction on the
	// console's own pool.
	fleetWriteTimeout = 10 * time.Second
)

// Why an organisation could not be reached. A closed set, matching the
// unreachable_kind comment in migrations/console/0019_org_facts.up.sql.
const (
	kindUnregistered  = "unregistered"
	kindUnprovisioned = "unprovisioned"
	kindDNS           = "dns"
	kindDial          = "dial"
	kindTLS           = "tls"
	kindTimeout       = "timeout"
	kindRefused       = "refused"
	kindRPC           = "rpc"

	// kindCancelled is the sweep being stopped, not the organisation failing.
	// Never written: runFleetPoll drops a result carrying it.
	kindCancelled = "cancelled"
)

// fleetPoller holds what survives between sweeps.
type fleetPoller struct {
	// published is the label set the last sweep wrote, so the next one can
	// retire organisations that have gone. Touched only by the sweep
	// goroutine, so it needs no lock.
	published map[string]bool
}

// runFleetPoller sweeps the fleet until ctx is cancelled.
//
// A timer rather than a ticker: the first interval differs from the rest, and
// resetting after the work makes the interval the gap between sweeps, so two
// sweeps can never overlap and nothing has to guard against it.
//
// ctx is the server's background context. Close cancels it and then closes the
// pool without waiting, so every call below descends from it.
//
// A sweep in flight when Close runs is not joined: its workers stop at their
// next network call, up to fleetOrgTimeout later. The process exits first in a
// deployment. Its results are dropped rather than recorded, since a
// cancellation is not a fact about any organisation.
func (s *Server) runFleetPoller(ctx context.Context) {
	p := &fleetPoller{published: map[string]bool{}}
	timer := time.NewTimer(firstFleetPollDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.runFleetPoll(ctx, p)
		timer.Reset(s.fleetPollInterval())
	}
}

func (s *Server) fleetPollInterval() time.Duration {
	if s.cfg.FleetPollInterval <= 0 {
		return defaultFleetPollInterval
	}
	return s.cfg.FleetPollInterval
}

func (s *Server) fleetOrgTimeout() time.Duration {
	if s.cfg.FleetOrgTimeout <= 0 {
		return defaultFleetOrgTimeout
	}
	return s.cfg.FleetOrgTimeout
}

// runFleetPoll visits every registered organisation once.
//
// Three phases. The list is read and its cursor closed; the organisations are
// visited in parallel, each writing one slot of a pre-sized slice so nothing
// needs a lock; then the results are written and published from this goroutine,
// so the gauge set is coherent and the retire step sees a complete sweep.
func (s *Server) runFleetPoll(ctx context.Context, p *fleetPoller) {
	started := time.Now()
	sweepCtx, cancel := context.WithTimeout(ctx, s.fleetSweepTimeout())
	defer cancel()

	orgs, err := s.db.listOrgNames(sweepCtx)
	if err != nil {
		if ctx.Err() == nil {
			fleetSweepFailures.WithLabelValues("list").Inc()
			s.log.Error("fleet sweep: list organisations", "err", err)
		}
		return
	}
	if len(orgs) == 0 {
		fleetOrgs.Set(0)
		retireOrgGauges(p.published, nil)
		p.published = map[string]bool{}
		return
	}

	facts := make([]orgFacts, len(orgs))
	sem := make(chan struct{}, fleetConcurrency)
	var wg sync.WaitGroup
	for i, org := range orgs {
		wg.Add(1)
		go func(i int, org string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			orgCtx, orgCancel := context.WithTimeout(sweepCtx, s.fleetOrgTimeout())
			defer orgCancel()
			facts[i] = s.collectOrgFacts(orgCtx, org)
		}(i, org)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return
	}

	// One deadline for every write, not one per organisation. A per-row budget
	// runs N of them in series outside the sweep's own, so a slow database
	// turns 128 organisations into twenty minutes of writes after a
	// four-minute sweep.
	writeCtx, writeCancel := context.WithTimeout(ctx, fleetWriteTimeout)
	defer writeCancel()

	seen := make(map[string]bool, len(facts))
	unreachable := 0
	for _, f := range facts {
		seen[f.Org] = true
		// Stopped mid-sweep: kept in seen so its gauges are not retired, and
		// neither published nor written, since a cancellation is not a fact
		// about the organisation.
		if f.UnreachableKind == kindCancelled {
			continue
		}
		if !f.Reachable {
			unreachable++
		}
		publishOrgFacts(f)

		if err := s.db.forOrg(f.Org).upsertOrgFacts(writeCtx, f); err != nil && ctx.Err() == nil {
			fleetSweepFailures.WithLabelValues("write").Inc()
			s.log.Error("fleet sweep: record facts", "org", f.Org, "err", err)
		}
	}

	retireOrgGauges(p.published, seen)
	p.published = seen

	fleetOrgs.Set(float64(len(facts)))
	fleetSweepSeconds.Observe(time.Since(started).Seconds())

	// Said only when something is wrong. A line every five minutes reporting a
	// healthy fleet is a line nobody reads.
	if unreachable > 0 {
		s.log.Info("fleet sweep finished", "orgs", len(facts),
			"unreachable", unreachable, "seconds", time.Since(started).Seconds())
	}
}

// fleetSweepTimeout keeps a sweep inside the tick that started it.
func (s *Server) fleetSweepTimeout() time.Duration {
	return s.fleetPollInterval() * fleetSweepNumerator / fleetSweepDenominator
}

// collectOrgFacts visits one organisation.
//
// The health listener first, and its failure is what decides reachability: it
// is a plain HTTP client, so a TLS or dial failure unwraps to the concrete
// type. gRPC folds a handshake failure into codes.Unavailable with a formatted
// message and a broken error chain, from which the cause cannot be recovered.
//
// A transport failure there ends the organisation. Without that a dead tenant
// spends four timeouts learning the same thing.
func (s *Server) collectOrgFacts(ctx context.Context, org string) orgFacts {
	f := orgFacts{Org: org, CollectedAt: time.Now().UTC()}

	entry, err := s.orgs.get(ctx, org)
	if err != nil {
		f.UnreachableKind, f.LastError = classifyUnreachable(err)
		return f
	}

	if err := s.readOrgStatus(ctx, entry, &f); err != nil {
		f.UnreachableKind, f.LastError = classifyUnreachable(err)
		// The cached channel has just been proved unusable, and nothing else
		// evicts on a transport failure — the entry is rebuilt only when the
		// registry row's updated_at moves. The next sweep dials afresh.
		if f.UnreachableKind == kindTLS {
			s.orgs.evict(org)
		}
		return f
	}
	f.Reachable = true

	// facts_at moves only when every fact was read. A partial read still
	// updates the columns it got, and leaves facts_at where it was, so the gap
	// to collected_at says some of the numbers are older than this sweep.
	//
	// Set on /status alone, that gap is zero on exactly the tenants a
	// mixed-version fleet produces: status answering, RPCs refusing.
	complete := true

	atl := entry.client
	if resp, err := atl.ListDeadJobs(ctx, &adminpb.ListDeadJobsRequest{Limit: fleetListLimit}); err != nil {
		complete = false
		s.noteFactError(&f, "dead jobs", org, err)
	} else {
		n := int32(len(resp.GetJobs()))
		truncated := n >= fleetListLimit
		f.DeadJobs, f.DeadJobsTruncated = &n, &truncated
	}

	if resp, err := atl.ListParkedObjects(ctx, &adminpb.ListParkedObjectsRequest{
		Limit: fleetListLimit,
	}); err != nil {
		complete = false
		s.noteFactError(&f, "parked objects", org, err)
	} else {
		objects := resp.GetObjects()
		n := int32(len(objects))
		overdue := countOverdue(objects, f.CollectedAt)
		truncated := resp.GetHasMore()
		f.ParkedObjects, f.ParkedOverdue, f.ParkedTruncated = &n, &overdue, &truncated
	}

	if resp, err := atl.ListFreezeWindows(ctx, &adminpb.ListFreezeWindowsRequest{}); err != nil {
		complete = false
		s.noteFactError(&f, "freeze windows", org, err)
	} else {
		open, endsAt := freezeOpenAt(resp.GetWindows(), f.CollectedAt)
		f.FreezeOpen = &open
		if open {
			f.FreezeEndsAt = &endsAt
		}
	}

	f.MeasuredFacts = complete
	return f
}

// noteFactError records one fact that could not be read.
//
// The organisation answered, so it stays reachable and the column stays NULL:
// a tenant older than an RPC reports Unimplemented, which is a gap in one fact
// and not an outage. Three organisations were once found running three
// different builds at once.
func (s *Server) noteFactError(f *orgFacts, what, org string, err error) {
	kind, text := classifyUnreachable(err)
	if kind == kindCancelled {
		// A stopping sweep, not a gap in this organisation's facts: marked so
		// runFleetPoll drops the whole result, and not logged, since a console
		// shutting down mid-sweep would otherwise warn three times per tenant.
		f.UnreachableKind = kindCancelled
		return
	}
	if f.LastError == "" {
		f.LastError = what + ": " + text
	}
	s.log.Warn("fleet sweep: a fact could not be read",
		"org", org, "fact", what, "kind", kind, "err", err)
}

// readOrgStatus fills the version fields from the health listener.
//
// The request carries the context so a cancelled sweep reaches it; the health
// client's own Timeout is client-wide and would not.
func (s *Server) readOrgStatus(ctx context.Context, e *orgClientEntry, f *orgFacts) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+e.health+"/status", nil)
	if err != nil {
		return err
	}
	resp, err := e.healthClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &statusError{code: resp.StatusCode}
	}

	var body struct {
		StartedAt     string `json:"started_at"`
		Version       string `json:"version"`
		SchemaVersion int64  `json:"schema_version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024)).Decode(&body); err != nil {
		return err
	}
	f.ServerVersion = body.Version
	// Absent below the first applied migration, which statusHandler omits
	// rather than sending as zero.
	v := body.SchemaVersion
	f.SchemaVersion = &v
	if t, err := time.Parse(time.RFC3339, body.StartedAt); err == nil {
		f.StartedAt = &t
	}
	return nil
}

// statusError is a health listener that answered with something other than 200.
type statusError struct{ code int }

func (e *statusError) Error() string {
	return "status endpoint answered " + http.StatusText(e.code)
}

// countOverdue counts parked objects whose retention has run out.
//
// The count of everything parked says how much is recoverable; this says how
// much should already have gone, which is what the register is read for.
func countOverdue(objects []*adminpb.ParkedObject, now time.Time) int32 {
	var n int32
	for _, o := range objects {
		if o.GetReapedAt() != "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, o.GetReapAfter())
		if err != nil {
			continue
		}
		if t.Before(now) {
			n++
		}
	}
	return n
}

// freezeOpenAt reports whether a window covers now, and when the last of the
// covering windows ends.
//
// ListFreezeWindows returns every row, including expired ones, and has no
// notion of the current time, so this is the whole of the "is a freeze on"
// question. The latest end among the covering windows is what says when it
// lifts; an earlier one is superseded.
func freezeOpenAt(windows []*adminpb.FreezeWindow, now time.Time) (bool, time.Time) {
	var (
		open   bool
		endsAt time.Time
	)
	for _, w := range windows {
		start, err := time.Parse(time.RFC3339, w.GetStartsAt())
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, w.GetEndsAt())
		if err != nil {
			continue
		}
		if now.Before(start) || !now.Before(end) {
			continue
		}
		open = true
		if end.After(endsAt) {
			endsAt = end
		}
	}
	return open, endsAt
}

// classifyUnreachable names the cause and returns the text to record.
//
// Nothing else in this package reads a gRPC code: every handler's failure
// becomes a 502 carrying the raw string, under which a pod that is down, a
// certificate that expired and a tenant refusing this console's credential are
// the same message. They call for different work, so they are separated here.
func classifyUnreachable(err error) (kind, text string) {
	text = err.Error()
	if len(text) > maxFactError {
		text = text[:maxFactError]
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return kindUnregistered, text
	case errors.Is(err, ErrOrgNotProvisioned):
		// An organisation exists in the registry from the first sign-in, before
		// anything provisions a stack for it. An expected state, not a fault.
		return kindUnprovisioned, text
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return kindTimeout, text
	case errors.Is(err, context.Canceled):
		// The sweep was stopped, which says nothing about the organisation.
		return kindCancelled, text
	}

	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return kindTLS, text
	}
	// Read as text, and before net.OpError below. A peer rejecting this
	// console's client certificate arrives as a TLS alert inside an OpError
	// with Op "remote error" — which the OpError branch would report as a pod
	// that is down. The gRPC transport also folds a handshake failure into
	// Unavailable with the cause only in the message.
	if strings.Contains(text, "x509:") || strings.Contains(text, "tls:") {
		return kindTLS, text
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return kindDNS, text
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return kindDial, text
	}

	switch status.Code(err) {
	case codes.Canceled:
		return kindCancelled, text
	case codes.Unauthenticated, codes.PermissionDenied:
		// The tenant answered and refused this console. A misconfiguration on
		// that organisation, which looks nothing like an outage.
		return kindRefused, text
	case codes.Unavailable:
		return kindDial, text
	case codes.DeadlineExceeded:
		return kindTimeout, text
	}
	return kindRPC, text
}
