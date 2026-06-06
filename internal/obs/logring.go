package obs

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"
)

// LogRing is a lock-free, fixed-size, multi-producer / single-or-multi-
// consumer ring buffer of slog records. It is the source of truth for
// the console's Health page log tail.
//
// # Hot-path latency
//
// Every slog.Info / Warn / Error call on the server flows through the
// tee handler that drives this ring. atlantis serves admin RPCs at
// millisecond-level latencies, so the append path must be:
//
//   - Non-blocking (no mutex contention can spike RPC P99 latencies).
//   - Allocation-bounded (a fresh Record per emit, garbage-collected as
//     the ring overwrites; no map allocations on the hot path).
//   - Cache-friendly: single atomic.Add to claim a slot, one
//     atomic.Pointer.Store to publish.
//
// Per-emit cost on a modern x86 is roughly 30-60 ns for the ring step
// (atomic.Add + atomic.Store + small struct write). The slog
// JSON/Text handler that emits to stdout is the dominant cost
// (~500 ns - 2 µs), unchanged from before. See logring_bench_test.go.
//
// # Drop semantics
//
// When the ring is full, new writes overwrite the oldest slot. A slow
// reader can never back-pressure a fast writer — by design. The reader
// detects skipped sequences by comparing the slot's recorded Seq to
// the index it is reading; a mismatch means that slot was overwritten
// and is silently skipped.
//
// # Read path
//
// Since() takes a cursor and returns every record with Seq > since,
// bounded by the ring's capacity. The reader takes a snapshot of head
// with one atomic.Load, then walks slot-by-slot. Slots being written
// concurrently may appear with Seq < expected (older record published
// before the writer started); those are skipped. The output is sorted
// by Seq before return — necessary because concurrent writers may have
// claimed slots out of order with respect to the head increment.
type LogRing struct {
	mask  uint64
	slots []atomic.Pointer[Record]
	head  atomic.Uint64
}

// Record is the immutable, value-typed log entry stored in the ring.
// Records are publish-once: a writer constructs a Record on the heap,
// fills it, and stores the pointer into a slot. Readers load the
// pointer atomically and treat the contents as read-only.
//
// Attrs are stored as a small slice of key/value pairs to avoid the
// per-emit allocations of a Go map (a slog handler typically attaches
// 2-6 attrs; a map costs 5+ allocations).
type Record struct {
	Seq   uint64
	Time  time.Time
	Level slog.Level
	Msg   string
	Attrs []KV
}

// KV is a single attribute key/value pair, with the value pre-rendered
// to string so the reader doesn't depend on slog.Value internals.
type KV struct {
	Key, Val string
}

// NewLogRing returns a ring sized to the smallest power of two ≥ minSize
// (minimum 2 so the mask is meaningful; capped at 1<<20 = ~1M slots to
// keep memory bounded).
func NewLogRing(minSize int) *LogRing {
	if minSize < 2 {
		minSize = 2
	}
	size := uint64(1)
	for size < uint64(minSize) {
		size <<= 1
	}
	if size > 1<<20 {
		size = 1 << 20
	}
	return &LogRing{
		mask:  size - 1,
		slots: make([]atomic.Pointer[Record], size),
	}
}

// Capacity returns the fixed number of slots in the ring.
func (r *LogRing) Capacity() int { return len(r.slots) }

// Append publishes rec into the ring. rec.Seq is assigned by Append.
// Safe for concurrent callers — the only synchronization is two atomic
// operations (no locks).
func (r *LogRing) Append(rec *Record) {
	// atomic.Add returns the post-increment value; subtracting 1 gives
	// the 0-indexed slot this writer "won."
	seq := r.head.Add(1)
	rec.Seq = seq
	r.slots[seq&r.mask].Store(rec)
}

// HeadSeq returns the highest sequence number ever assigned. Useful
// for clients that want to know "have I caught up?" without pulling
// the records themselves.
func (r *LogRing) HeadSeq() uint64 { return r.head.Load() }

// Since returns every record with Seq > since, in ascending Seq order,
// along with the snapshotted head. The size of the returned slice is
// bounded by the ring's capacity — older records are gone.
//
// The cost is O(N) where N is min(capacity, head-since). This is a
// pull-side cost paid only when the console polls; the hot writer
// path is untouched.
func (r *LogRing) Since(since uint64) (recs []Record, head uint64) {
	head = r.head.Load()
	if head <= since {
		return nil, head
	}

	// Don't walk further back than the ring's window. Anything more
	// than `capacity` records behind head has been overwritten.
	earliest := uint64(0)
	capU := uint64(len(r.slots))
	if head > capU {
		earliest = head - capU + 1
	} else {
		earliest = 1
	}
	if earliest <= since {
		earliest = since + 1
	}

	// First pass: collect candidate records.
	out := make([]Record, 0, head-earliest+1)
	for i := earliest; i <= head; i++ {
		p := r.slots[i&r.mask].Load()
		if p == nil {
			continue
		}
		// Slot must currently hold the record we expect — if a later
		// writer has wrapped around and overwritten this index with a
		// higher Seq, our expected record is gone.
		if p.Seq != i {
			continue
		}
		out = append(out, *p)
	}

	// Concurrent writers can publish slots out of strict order; sort
	// to make the cursor semantics correct.
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, head
}

// ---------------------------------------------------------------------------
// slog.Handler tee
// ---------------------------------------------------------------------------

// RingHandler is a slog.Handler that forwards every Record to a
// downstream handler (typically the existing JSON or Text handler
// writing to stdout) AND publishes a copy into a LogRing for the
// console's log tail.
//
// The downstream handler runs first and unchanged; the ring publish
// step happens after. If the downstream handler panics or errors,
// the ring is not updated for that record. (Errors are surfaced to
// the slog caller, matching base Handler semantics.)
type RingHandler struct {
	next slog.Handler
	ring *LogRing
}

// NewRingHandler wraps next so every record it sees is also captured
// into ring. Pass the existing stderr/JSON handler as next and the
// behavior of every existing slog call site is preserved.
func NewRingHandler(next slog.Handler, ring *LogRing) *RingHandler {
	return &RingHandler{next: next, ring: ring}
}

func (h *RingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.next.Enabled(ctx, lvl)
}

func (h *RingHandler) Handle(ctx context.Context, rec slog.Record) error {
	// 1. Forward to the downstream handler first so its output is the
	//    canonical record (stdout, journald, etc).
	if err := h.next.Handle(ctx, rec); err != nil {
		return err
	}

	// 2. Capture into the ring. Single allocation per emit.
	out := &Record{
		Time:  rec.Time,
		Level: rec.Level,
		Msg:   rec.Message,
		Attrs: make([]KV, 0, rec.NumAttrs()),
	}
	rec.Attrs(func(a slog.Attr) bool {
		out.Attrs = append(out.Attrs, KV{Key: a.Key, Val: a.Value.String()})
		return true
	})
	h.ring.Append(out)
	return nil
}

func (h *RingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RingHandler{next: h.next.WithAttrs(attrs), ring: h.ring}
}

func (h *RingHandler) WithGroup(name string) slog.Handler {
	return &RingHandler{next: h.next.WithGroup(name), ring: h.ring}
}
