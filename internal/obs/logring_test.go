package obs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestLogRing_SequentialAppendThenSinceReturnsAll(t *testing.T) {
	r := NewLogRing(64)
	for i := 0; i < 10; i++ {
		r.Append(&Record{Time: time.Unix(0, int64(i)), Msg: "x"})
	}
	recs, head := r.Since(0)
	if head != 10 {
		t.Fatalf("head=%d, want 10", head)
	}
	if len(recs) != 10 {
		t.Fatalf("got %d records, want 10", len(recs))
	}
	for i, rec := range recs {
		if rec.Seq != uint64(i+1) {
			t.Errorf("recs[%d].Seq=%d, want %d", i, rec.Seq, i+1)
		}
	}
}

func TestLogRing_SinceCursorOnlyReturnsNewer(t *testing.T) {
	r := NewLogRing(64)
	for i := 0; i < 10; i++ {
		r.Append(&Record{Msg: "x"})
	}
	recs, _ := r.Since(5)
	if len(recs) != 5 {
		t.Fatalf("got %d, want 5", len(recs))
	}
	if recs[0].Seq != 6 || recs[4].Seq != 10 {
		t.Fatalf("got Seq range [%d, %d], want [6, 10]", recs[0].Seq, recs[4].Seq)
	}
}

func TestLogRing_OverwriteDropsOldest(t *testing.T) {
	r := NewLogRing(8) // tiny ring → wrap after 8
	for i := 0; i < 20; i++ {
		r.Append(&Record{Msg: "x"})
	}
	recs, head := r.Since(0)
	if head != 20 {
		t.Fatalf("head=%d, want 20", head)
	}
	// Only the last 8 should be readable.
	if len(recs) != 8 {
		t.Fatalf("got %d records, want 8 (ring size)", len(recs))
	}
	if recs[0].Seq != 13 || recs[7].Seq != 20 {
		t.Fatalf("got Seq range [%d, %d], want [13, 20]", recs[0].Seq, recs[7].Seq)
	}
}

func TestLogRing_SinceBeyondHeadReturnsEmpty(t *testing.T) {
	r := NewLogRing(64)
	for i := 0; i < 5; i++ {
		r.Append(&Record{Msg: "x"})
	}
	recs, head := r.Since(100)
	if head != 5 {
		t.Fatalf("head=%d, want 5", head)
	}
	if recs != nil {
		t.Fatalf("got %d records, want nil", len(recs))
	}
}

func TestLogRing_PowerOfTwoSizing(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, 2},
		{1, 2},
		{2, 2},
		{3, 4},
		{8, 8},
		{64, 64},
		{65, 128},
		{500, 512},
		{1024, 1024},
		{1025, 2048},
	}
	for _, c := range cases {
		r := NewLogRing(c.in)
		if r.Capacity() != c.want {
			t.Errorf("NewLogRing(%d).Capacity()=%d, want %d", c.in, r.Capacity(), c.want)
		}
	}
}

func TestLogRing_ConcurrentAppendNoDuplicateSeq(t *testing.T) {
	r := NewLogRing(8192)
	const writers = 8
	const perWriter = 1000

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				r.Append(&Record{Msg: "x"})
			}
		}()
	}
	wg.Wait()

	if got, want := r.HeadSeq(), uint64(writers*perWriter); got != want {
		t.Fatalf("HeadSeq()=%d, want %d", got, want)
	}

	recs, _ := r.Since(0)
	if len(recs) != writers*perWriter {
		t.Fatalf("got %d records, want %d", len(recs), writers*perWriter)
	}
	seen := make(map[uint64]bool, len(recs))
	for _, rec := range recs {
		if seen[rec.Seq] {
			t.Fatalf("duplicate Seq=%d", rec.Seq)
		}
		seen[rec.Seq] = true
	}
}

// ---------------------------------------------------------------------------
// RingHandler tee tests
// ---------------------------------------------------------------------------

func TestRingHandler_TeeForwardsToDownstreamAndRing(t *testing.T) {
	var buf bytes.Buffer
	ring := NewLogRing(64)
	next := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := NewRingHandler(next, ring)

	log := slog.New(h)
	log.Info("hello", "caller", "backend", "ms", 47)

	// Downstream got the line.
	if buf.Len() == 0 {
		t.Fatal("downstream handler produced no output")
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"msg":"hello"`)) {
		t.Errorf("downstream output missing msg: %s", buf.String())
	}

	// Ring got the record.
	recs, _ := ring.Since(0)
	if len(recs) != 1 {
		t.Fatalf("ring got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Msg != "hello" {
		t.Errorf("ring Msg=%q, want %q", r.Msg, "hello")
	}
	if r.Level != slog.LevelInfo {
		t.Errorf("ring Level=%v, want Info", r.Level)
	}
	if len(r.Attrs) != 2 {
		t.Fatalf("ring Attrs=%d, want 2", len(r.Attrs))
	}
	gotAttrs := map[string]string{r.Attrs[0].Key: r.Attrs[0].Val, r.Attrs[1].Key: r.Attrs[1].Val}
	if gotAttrs["caller"] != "backend" {
		t.Errorf("attr caller=%q, want backend", gotAttrs["caller"])
	}
	if gotAttrs["ms"] != "47" {
		t.Errorf("attr ms=%q, want 47", gotAttrs["ms"])
	}
}

func TestRingHandler_WithAttrsAppliesDownstream(t *testing.T) {
	var buf bytes.Buffer
	ring := NewLogRing(64)
	next := slog.NewJSONHandler(&buf, nil)
	h := NewRingHandler(next, ring)

	// WithAttrs should produce a derived handler that scopes the attr
	// onto every subsequent record (per slog.Handler contract). The
	// derived handler must still publish into the same ring.
	log := slog.New(h).With("service", "atlantis")
	log.Info("boot")

	if !bytes.Contains(buf.Bytes(), []byte(`"service":"atlantis"`)) {
		t.Errorf("With() attrs not propagated to downstream: %s", buf.String())
	}
	recs, _ := ring.Since(0)
	if len(recs) != 1 {
		t.Fatalf("ring got %d records, want 1", len(recs))
	}
}

func TestRingHandler_RespectsEnabledFromDownstream(t *testing.T) {
	ring := NewLogRing(64)
	next := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := NewRingHandler(next, ring)

	log := slog.New(h)
	log.Debug("noise") // below WARN threshold → suppressed at slog layer
	log.Info("noise")
	log.Warn("important")

	recs, _ := ring.Since(0)
	if len(recs) != 1 {
		t.Fatalf("ring got %d records, want 1 (only Warn)", len(recs))
	}
	if recs[0].Msg != "important" {
		t.Errorf("ring captured wrong msg: %q", recs[0].Msg)
	}
}

func TestRingHandler_ContextPassedThrough(t *testing.T) {
	ring := NewLogRing(64)
	next := slog.NewJSONHandler(io.Discard, nil)
	h := NewRingHandler(next, ring)

	ctx := context.Background()
	log := slog.New(h)
	log.InfoContext(ctx, "ctx-aware")

	recs, _ := ring.Since(0)
	if len(recs) != 1 || recs[0].Msg != "ctx-aware" {
		t.Fatalf("ring did not capture context-aware emit")
	}
}
