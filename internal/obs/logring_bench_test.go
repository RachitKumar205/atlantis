package obs

import (
	"io"
	"log/slog"
	"sync"
	"testing"
)

// BenchmarkRing_AppendSingleGoroutine bounds the hottest possible
// case: one writer, no contention. The number to watch is ns/op —
// for the millisecond-latency claim to hold this must stay << 1µs.
func BenchmarkRing_AppendSingleGoroutine(b *testing.B) {
	r := NewLogRing(8192)
	rec := &Record{Msg: "hot path"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Allocation-free path: re-using a record pointer. In real
		// usage RingHandler allocates one Record per emit; that's
		// measured in BenchmarkRingHandler_End2End below.
		r.Append(rec)
	}
}

// BenchmarkRing_AppendContended hammers the ring from GOMAXPROCS
// writers. Critical: with a mutex this would degrade non-linearly;
// with the lock-free path it should scale flatly per goroutine.
func BenchmarkRing_AppendContended(b *testing.B) {
	r := NewLogRing(8192)
	rec := &Record{Msg: "hot path"}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Append(rec)
		}
	})
}

// BenchmarkRingHandler_End2End measures the slog.Handler tee under
// realistic conditions: each Info() allocates a Record, walks attrs,
// publishes to ring, AND runs the downstream JSON handler writing to
// /dev/null. The marginal cost of the ring step is the gap vs.
// BenchmarkSlogHandlerOnly below.
func BenchmarkRingHandler_End2End(b *testing.B) {
	ring := NewLogRing(8192)
	next := slog.NewJSONHandler(io.Discard, nil)
	log := slog.New(NewRingHandler(next, ring))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.Info("apply committed", "caller", "backend", "version", 48, "ms", 47)
	}
}

// BenchmarkSlogHandlerOnly is the baseline: same JSON handler writing
// to /dev/null, no ring. Subtract its ns/op from
// BenchmarkRingHandler_End2End to attribute the ring cost.
func BenchmarkSlogHandlerOnly(b *testing.B) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.Info("apply committed", "caller", "backend", "version", 48, "ms", 47)
	}
}

// BenchmarkRingHandler_ContendedEnd2End is the realistic worst case:
// GOMAXPROCS goroutines all logging through the ring handler at once.
// This is what protects RPC tail latencies under concurrent load.
func BenchmarkRingHandler_ContendedEnd2End(b *testing.B) {
	ring := NewLogRing(8192)
	next := slog.NewJSONHandler(io.Discard, nil)
	log := slog.New(NewRingHandler(next, ring))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			log.Info("apply committed", "caller", "backend", "version", 48)
		}
	})
}

// BenchmarkRing_SinceLargeWindow measures the read path. This runs
// once per console poll (~1 Hz), not on the hot writer path, so the
// budget is much more relaxed — but we still want to know.
func BenchmarkRing_SinceLargeWindow(b *testing.B) {
	r := NewLogRing(8192)
	for i := 0; i < 8000; i++ {
		r.Append(&Record{Msg: "x"})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = r.Since(0)
	}
}

// Compile-time check: stress test that simultaneous Append + Since
// don't tear records (race detector required: go test -race).
func TestRingHandler_ConcurrentAppendAndSince(t *testing.T) {
	ring := NewLogRing(1024)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(4)
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					ring.Append(&Record{Msg: "x"})
				}
			}
		}()
	}

	// Concurrent reader.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = ring.Since(0)
			}
		}
	}()

	// Let them run briefly.
	for i := 0; i < 100; i++ {
		_, _ = ring.Since(0)
	}
	close(stop)
	wg.Wait()
}
