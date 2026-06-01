package controller

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// TestConcurrentPickApplyAndSnapshot exercises the A8 lock-free snapshot path
// against a hot Pick+Apply pipeline. One goroutine drives the controller as
// the lifecycle does (Pick under LockState, then Apply under LockState); a
// second goroutine pulls FSnapshot in a tight loop *without* any lock, the
// way batch.go::buildRecord now does. Under `go test -race` the run must
// produce zero race-detector hits: the snapshot reads each VerbRow's
// AxisVec slot independently, so a torn read against an Apply mutator
// resolves to either the pre- or post-Apply value per slot — both valid —
// and the Rows slice header is fixed-length post-NewState so there is no
// slice-growth race either.
//
// 1s wall budget keeps the test fast in CI while still exercising O(10⁵+)
// Pick/Apply/snapshot interleavings.
func TestConcurrentPickApplyAndSnapshot(t *testing.T) {
	verbs := []string{"eoatx", "storagespam", "deploytx", "uniswap_swaps", "calltx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":         {"accounts": 160, "storage": 5, "code": 0},
			"storagespam":   {"accounts": 5, "storage": 220, "code": 0},
			"deploytx":      {"accounts": 100, "storage": 0, "code": 3500},
			"uniswap_swaps": {"accounts": 8, "storage": 220, "code": 0},
			"calltx":        {"accounts": 5, "storage": 80, "code": 0},
		},
		AvgTxRLP: map[string]float64{
			"eoatx": 1500, "storagespam": 1500, "deploytx": 1500,
			"uniswap_swaps": 1500, "calltx": 1500,
		},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	tgt := makeTarget(10 * 1024 * 1024 * 1024)
	obs := &Observation{
		AccountTrieBytes: 100_000_000,
		StorageTrieBytes: 100_000_000,
		CodeBytesTotal:   10_000_000,
	}
	plan := &BatchPlan{Verb: "eoatx", DeadlineBytes: 1500 * 100, Mix: map[string]float64{"eoatx": 1.0}}
	post := &Observation{
		AccountTrieBytes: 100_001_000,
		StorageTrieBytes: 100_000_500,
		CodeBytesTotal:   10_000_100,
	}

	deadline := time.Now().Add(1 * time.Second)
	var stop atomic.Bool

	var wg sync.WaitGroup
	var pickApplyOps, snapshotOps atomic.Uint64
	var sawNonFinite atomic.Bool

	// Writer: emulate the committer-goroutine cadence — Pick then Apply, both
	// under LockState. A real lifecycle alternates them across goroutines; for
	// the race-detector contract the snapshot reader is what we need to keep
	// strictly lock-free, so colocating Pick+Apply in one writer goroutine is
	// fine and exercises the same memory orderings.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			s.LockState()
			_ = s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
			_, _ = s.Apply(obs, post, plan, 100, 150000, 2_500_000_000)
			s.UnlockState()
			pickApplyOps.Add(1)
		}
	}()

	// Reader: pull FSnapshot in a tight loop with NO LockState, the way the
	// post-A8 batch.go::buildRecord does. Every value must be finite — a
	// non-finite read would imply a torn write produced garbage, which the
	// per-slot word-atomic invariant must prevent.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			snap := s.FSnapshot()
			for _, row := range snap {
				for _, v := range row {
					if math.IsNaN(v) || math.IsInf(v, 0) {
						sawNonFinite.Store(true)
					}
				}
			}
			snapshotOps.Add(1)
		}
	}()

	time.Sleep(time.Until(deadline))
	stop.Store(true)
	wg.Wait()

	if sawNonFinite.Load() {
		t.Fatal("snapshot reader observed a non-finite F coefficient — torn write?")
	}
	if pickApplyOps.Load() == 0 {
		t.Fatal("writer never ran a Pick+Apply cycle — test setup broken")
	}
	if snapshotOps.Load() == 0 {
		t.Fatal("reader never ran an FSnapshot — test setup broken")
	}
	// Sanity: with 1s budget on the writer's hot path we expect at least a few
	// thousand iterations of each goroutine; the lower bound just guards
	// against a completely-stalled goroutine.
	if pickApplyOps.Load() < 100 || snapshotOps.Load() < 100 {
		t.Errorf("pickApply=%d snapshot=%d — fewer iterations than expected, both should exceed 100",
			pickApplyOps.Load(), snapshotOps.Load())
	}
	t.Logf("pickApply iterations=%d snapshot iterations=%d", pickApplyOps.Load(), snapshotOps.Load())
}

// BenchmarkPick is the A8 alloc-per-op witness. The bench must report zero
// allocs/op for the steady-state Pick call (after the first warm-up call has
// allocated the BatchPlan.Mix map — that one map is the only allocation that
// cannot be hoisted to NewState because BatchPlan escapes to the committer).
// `-benchmem` exposes the alloc count.
func BenchmarkPick(b *testing.B) {
	verbs := []string{"eoatx", "storagespam", "deploytx", "uniswap_swaps", "calltx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":         {"accounts": 160, "storage": 5, "code": 0},
			"storagespam":   {"accounts": 5, "storage": 220, "code": 0},
			"deploytx":      {"accounts": 100, "storage": 0, "code": 3500},
			"uniswap_swaps": {"accounts": 8, "storage": 220, "code": 0},
			"calltx":        {"accounts": 5, "storage": 80, "code": 0},
		},
		AvgTxRLP: map[string]float64{
			"eoatx": 1500, "storagespam": 1500, "deploytx": 1500,
			"uniswap_swaps": 1500, "calltx": 1500,
		},
	}
	var identity [32]byte
	s := newTestStateEps(verbs, rf, identity, 0)

	tgt := makeTarget(10 * 1024 * 1024 * 1024)
	obs := &Observation{
		AccountTrieBytes: 100_000_000,
		StorageTrieBytes: 100_000_000,
		CodeBytesTotal:   10_000_000,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	}
}

// BenchmarkPickPreA8Baseline simulates the pre-A8 Pick allocation profile by
// re-allocating the five scratch buffers (fMat triple, score, maxNTxs,
// candidates, mix) every call. It is NOT a true historical benchmark — it
// stands in for the legacy code so the alloc-per-op delta the A8 refactor
// produces is visible in `-benchmem` without bisecting git history. A package
// sink keeps the escape analyzer from stack-allocating the buffers.
var allocSink struct {
	fMat       [3][]float64
	score      []float64
	maxNTxs    []float64
	candidates []int
	mix        map[string]float64
}

func BenchmarkPickPreA8Baseline(b *testing.B) {
	n := 5
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		allocSink.fMat = [3][]float64{make([]float64, n), make([]float64, n), make([]float64, n)}
		allocSink.score = make([]float64, n)
		allocSink.maxNTxs = make([]float64, n)
		allocSink.candidates = make([]int, 0, n)
		allocSink.mix = make(map[string]float64, n)
	}
}
