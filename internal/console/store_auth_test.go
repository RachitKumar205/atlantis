package console

import (
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TestDummyAuthHash_RealBcryptRound is the regression test for the
// timing-oracle fix in authenticateUser. The "user does not exist" path
// must call bcrypt.CompareHashAndPassword against a HASH THAT ACTUALLY
// PARSES so the bcrypt round runs. The original bug passed an empty
// string which short-circuited with ErrHashTooShort.
//
// We assert two things about dummyAuthHash:
//
//  1. It's a syntactically valid bcrypt hash (CompareHashAndPassword
//     returns ErrMismatchedHashAndPassword, NOT ErrHashTooShort).
//  2. Comparing against it takes meaningfully longer than the broken
//     "compare against empty string" path — confirming the bcrypt round
//     actually ran rather than short-circuiting on a format check.
func TestDummyAuthHash_RealBcryptRound(t *testing.T) {
	if dummyAuthHash == nil {
		t.Fatal("dummyAuthHash was not initialized at package init")
	}

	// (1) It must be a valid bcrypt hash that fails the password compare.
	err := bcrypt.CompareHashAndPassword(dummyAuthHash, []byte("not-the-placeholder"))
	if err == nil {
		t.Fatal("dummy hash compare succeeded with a wrong password")
	}
	if err == bcrypt.ErrHashTooShort {
		t.Fatalf("dummy hash returned ErrHashTooShort — bcrypt round did not run, timing oracle still present")
	}
	if err != bcrypt.ErrMismatchedHashAndPassword {
		t.Fatalf("unexpected error from dummy hash compare: %v", err)
	}

	// (2) The bcrypt round must actually take work — many milliseconds at
	// DefaultCost vs. microseconds for the broken empty-hash short-circuit.
	// We measure both paths and require the dummy path to be at LEAST 10x
	// the empty-hash path. A 100ms vs 1µs gap is the real-world ratio at
	// DefaultCost; the 10x bar is generous enough to pass on slow CI
	// without false-failing on noise.
	const samples = 5

	timeEmpty := medianDuration(samples, func() {
		_ = bcrypt.CompareHashAndPassword([]byte(""), []byte("anything"))
	})
	timeDummy := medianDuration(samples, func() {
		_ = bcrypt.CompareHashAndPassword(dummyAuthHash, []byte("anything"))
	})
	if timeDummy < timeEmpty*10 {
		t.Errorf("dummy bcrypt path (%v) is not meaningfully slower than empty-hash path (%v) — round did not run", timeDummy, timeEmpty)
	}
}

func medianDuration(n int, fn func()) time.Duration {
	durs := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		fn()
		durs[i] = time.Since(start)
	}
	// O(n^2) but n is 5 — clearer than pulling in sort.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if durs[i] > durs[j] {
				durs[i], durs[j] = durs[j], durs[i]
			}
		}
	}
	return durs[n/2]
}
