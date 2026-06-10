package jobs

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// newTestDispatchedWorker constructs a minimal DispatchedWorker with
// the channel cap structure we want to exercise. No gRPC stream
// involvement — the tests target the channel routing + checkpointer
// contract.
func newTestDispatchedWorker(t *testing.T) *DispatchedWorker {
	t.Helper()
	return &DispatchedWorker{
		cfg:    ServerConfig{MaxInFlight: 4, PodID: "test"},
		ctrlCh: make(chan *WorkerEnvelope, 4+8),
		dataCh: make(chan *WorkerEnvelope, 4*2),
	}
}

// newDispatchTestWorker builds a worker wired enough to drive
// handleDispatch: a registry, an inflight map, a silent logger, and
// generous channels.
func newDispatchTestWorker(t *testing.T, reg *Registry) *DispatchedWorker {
	t.Helper()
	return &DispatchedWorker{
		cfg:      ServerConfig{MaxInFlight: 4, PodID: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		registry: reg,
		inflight: make(map[int64]inflightHandle),
		ctrlCh:   make(chan *WorkerEnvelope, 64),
		dataCh:   make(chan *WorkerEnvelope, 64),
	}
}

// TestHandleDispatch_DuplicateIgnored pins the incident fix: a second
// Dispatch for a job id whose handler is already running must NOT spawn
// a second goroutine or inflate the in-flight count. It re-Acks (the
// server reset its ackBy clock) and returns.
func TestHandleDispatch_DuplicateIgnored(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	reg := NewRegistry()
	reg.Register("ns.Job", HandlerFunc(func(ctx context.Context, _ []byte) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // block so the job stays in-flight
		return nil
	}))
	w := newDispatchTestWorker(t, reg)

	w.handleDispatch(context.Background(), &Dispatch{JobID: 7, JobName: "ns.Job"})
	<-started // handler goroutine is running
	if got := w.Inflight(); got != 1 {
		t.Fatalf("after first dispatch, inflight = %d, want 1", got)
	}

	// Duplicate dispatch of the same id.
	w.handleDispatch(context.Background(), &Dispatch{JobID: 7, JobName: "ns.Job"})
	if got := w.Inflight(); got != 1 {
		t.Errorf("duplicate dispatch inflated inflight to %d, want 1", got)
	}

	// Exactly two Acks total (one per dispatch), no Complete yet.
	acks := drainAcks(w.dataCh)
	if acks != 2 {
		t.Errorf("got %d Acks, want 2 (one per dispatch)", acks)
	}

	close(release)
	// Let the single handler finish and emit exactly one Complete.
	waitInflightZero(t, w)
	if got := w.Inflight(); got != 0 {
		t.Errorf("after release, inflight = %d, want 0", got)
	}
}

func drainAcks(ch chan *WorkerEnvelope) int {
	n := 0
	for {
		select {
		case env := <-ch:
			if env.Ack != nil {
				n++
			}
		default:
			return n
		}
	}
}

func waitInflightZero(t *testing.T, w *DispatchedWorker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.Inflight() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("inflight did not reach 0 within 2s")
}

// TestStreamCheckpointer_RoutesViaCtrlCh pins the load-bearing
// guarantee: Checkpoint envelopes go through the priority control
// channel, never the data plane. If a future refactor pushes
// Checkpoint to dataCh, the stale-heartbeat regression bites again.
func TestStreamCheckpointer_RoutesViaCtrlCh(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 42)

	if err := c.Report(context.Background(), 50, "halfway"); err != nil {
		t.Fatalf("Report: %v", err)
	}

	select {
	case env := <-w.ctrlCh:
		if env.Checkpoint == nil {
			t.Fatalf("ctrlCh envelope missing Checkpoint payload")
		}
		if env.Checkpoint.JobID != 42 || env.Checkpoint.Pct != 50 || env.Checkpoint.Msg != "halfway" {
			t.Errorf("unexpected checkpoint payload: %+v", env.Checkpoint)
		}
	default:
		t.Fatal("expected checkpoint on ctrlCh")
	}

	select {
	case env := <-w.dataCh:
		t.Errorf("dataCh must NOT receive Checkpoint; got %+v", env)
	default:
	}
}

func TestStreamCheckpointer_ClampsPct(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 7)

	cases := []struct {
		in, want int
	}{
		{-5, 0},
		{0, 0},
		{50, 50},
		{100, 100},
		{1234, 100},
	}
	for _, tc := range cases {
		drainCtrl(w)
		_ = c.Report(context.Background(), tc.in, "x")
		env := <-w.ctrlCh
		if env.Checkpoint.Pct != tc.want {
			t.Errorf("Pct=%d clamped to %d, want %d",
				tc.in, env.Checkpoint.Pct, tc.want)
		}
	}
}

func TestStreamCheckpointer_TruncatesMsg(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 7)
	long := strings.Repeat("x", MaxCheckpointMsgChars+50)

	_ = c.Report(context.Background(), 50, long)
	env := <-w.ctrlCh
	if len(env.Checkpoint.Msg) != MaxCheckpointMsgChars {
		t.Errorf("Msg length = %d, want %d",
			len(env.Checkpoint.Msg), MaxCheckpointMsgChars)
	}
}

func TestCheckpoint_NoOpWithoutWorkerCtx(t *testing.T) {
	// The package-level Checkpoint function must be a no-op when no
	// Checkpointer is wired into the ctx — that's the "safe in unit
	// tests" contract documented on the API.
	if err := Checkpoint(context.Background(), 50, "msg"); err != nil {
		t.Errorf("Checkpoint on bare ctx should return nil; got %v", err)
	}
}

func TestCheckpoint_RoutesThroughInstalledCheckpointer(t *testing.T) {
	w := newTestDispatchedWorker(t)
	ctx := withCheckpointer(context.Background(), newStreamCheckpointer(w, 99))

	if err := Checkpoint(ctx, 25, "quarter"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	env := <-w.ctrlCh
	if env.Checkpoint.JobID != 99 || env.Checkpoint.Pct != 25 {
		t.Errorf("envelope routed wrong: %+v", env.Checkpoint)
	}
}

// drainCtrl removes any pending envelopes from ctrlCh between sub-test
// iterations of the clamping test.
func drainCtrl(w *DispatchedWorker) {
	for {
		select {
		case <-w.ctrlCh:
		default:
			return
		}
	}
}
