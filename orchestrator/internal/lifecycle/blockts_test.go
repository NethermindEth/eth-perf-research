package lifecycle

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNextBlockTSStrictlyIncreasing: repeated calls within the same wall-clock
// second still yield strictly-increasing timestamps (the +1 tie-break path).
func TestNextBlockTSStrictlyIncreasing(t *testing.T) {
	last := new(atomic.Uint64)
	const n = 10
	prev := nextBlockTS(last)
	for i := 1; i < n; i++ {
		got := nextBlockTS(last)
		if got <= prev {
			t.Fatalf("call %d: %d not > previous %d", i, got, prev)
		}
		prev = got
	}
}

// TestNextBlockTSRespectsSeed: the first result is strictly greater than the
// seeded value when the seed is in the future relative to wall clock.
func TestNextBlockTSRespectsSeed(t *testing.T) {
	seed := uint64(time.Now().Unix()) + 1_000_000
	last := new(atomic.Uint64)
	last.Store(seed)

	got := nextBlockTS(last)
	if got <= seed {
		t.Fatalf("first result %d not > seed %d", got, seed)
	}
	if got != seed+1 {
		t.Fatalf("first result %d != seed+1 %d", got, seed+1)
	}
}

// TestNextBlockTSUsesWallClockWhenAhead: when wall-clock has advanced past the
// last value, that wall-clock value is used (no spurious +1).
func TestNextBlockTSUsesWallClockWhenAhead(t *testing.T) {
	last := new(atomic.Uint64)
	last.Store(1) // far in the past
	now := uint64(time.Now().Unix())

	got := nextBlockTS(last)
	if got != now {
		t.Fatalf("expected wall-clock %d, got %d", now, got)
	}
}

// TestNextBlockTSConcurrent: concurrent callers never observe a duplicate or a
// regression — all returned timestamps are distinct and strictly increasing
// when sorted.
func TestNextBlockTSConcurrent(t *testing.T) {
	last := new(atomic.Uint64)
	last.Store(uint64(time.Now().Unix()))

	const goroutines = 8
	const perGoroutine = 50
	results := make([]uint64, goroutines*perGoroutine)

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perGoroutine {
				results[g*perGoroutine+i] = nextBlockTS(last)
			}
		}(g)
	}
	wg.Wait()

	seen := make(map[uint64]struct{}, len(results))
	for _, ts := range results {
		if _, dup := seen[ts]; dup {
			t.Fatalf("duplicate timestamp %d under concurrency", ts)
		}
		seen[ts] = struct{}{}
	}
}
