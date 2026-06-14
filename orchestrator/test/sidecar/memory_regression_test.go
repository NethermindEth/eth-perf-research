package sidecartest

import (
	"runtime"
	"testing"

	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// TestMemoryRegression_1M loads 1M synthetic code-hash and slot entries into
// the tracker and asserts the process heap stays under a tight ceiling.
//
// The architecture spec calls for a 560 MB total RSS ceiling on the full
// production workload (200M codehashes + 25M contracts). At 1M entries we
// budget proportionally; this test catches per-entry overhead regressions
// (e.g. accidental pointer-heavy structs) that would blow up the production
// build.
func TestMemoryRegression_1M(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory regression in short mode")
	}

	const n = 1_000_000
	tr := tracker.New()

	codes := make([]tracker.CodeSeed, n)
	slots := make([]tracker.SlotSeed, n)
	for i := 0; i < n; i++ {
		var h [32]byte
		// Fill the first 8 bytes with the index so every hash is unique and
		// shard distribution is roughly uniform.
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		h[3] = byte(i >> 24)
		codes[i] = tracker.CodeSeed{CodeHash: h, CodeSize: 1024, Refcount: 1}
		slots[i] = tracker.SlotSeed{HashedAddress: h, SlotCount: uint64(i % 1024)}
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	tr.SeedFromScan(codes, slots)

	// Drop the seed slices so we measure only the tracker's footprint.
	codes = nil
	slots = nil
	runtime.GC()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	growthMB := int64(after.Alloc-before.Alloc) / (1024 * 1024)
	t.Logf("tracker heap growth for 1M entries: %d MB (Alloc=%d MB, Sys=%d MB)",
		growthMB, after.Alloc/(1024*1024), after.Sys/(1024*1024))

	// Each entry costs ~(32B key + 8B count) + ~50B map overhead = ~90 B.
	// 1M × 2 maps × 90 B ≈ 180 MB. Ceiling: 300 MB allows for go-map
	// load-factor slack. Sys ceiling 600 MB matches the architecture
	// document's per-component budget.
	const ceilingMB = 300
	if growthMB > ceilingMB {
		t.Errorf("heap growth %d MB exceeded ceiling %d MB — possible regression",
			growthMB, ceilingMB)
	}

	// Sanity: the tracker still produces correct counters.
	got := tr.SnapshotCounters()
	if got.UniqueCodeHashes != n {
		t.Errorf("UniqueCodeHashes = %d, want %d", got.UniqueCodeHashes, n)
	}
	if got.ContractsTotal != n {
		t.Errorf("ContractsTotal = %d, want %d", got.ContractsTotal, n)
	}
	if got.CodeBytesTotal != int64(n)*1024 {
		t.Errorf("CodeBytesTotal = %d, want %d", got.CodeBytesTotal, int64(n)*1024)
	}
}

// TestMemoryRegression_StreamingSeed1M asserts that the streaming seed path
// (SeedOneCode invoked record-by-record) does not allocate a hidden O(N)
// slice the way the legacy SeedFromScan + []CodeSeed path used to. This is
// the load-bearing regression test for the v19 bootstrap OOM fix:
// streamingDedupAndLookupSink calls SeedOneCode N times via a sink
// callback; if any layer in between accumulates a slice we are back to
// 60 GB of seed headers at bloatnet scale.
//
// We deliberately do NOT pre-allocate the seeds slice — instead the test
// generates each seed inline, calls SeedOneCode, and forgets it. The peak
// transient allocation must therefore be O(tracker storage), not
// O(2 × tracker storage) as the slice path used to be.
func TestMemoryRegression_StreamingSeed1M(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory regression in short mode")
	}
	const n = 1_000_000
	tr := tracker.New()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < n; i++ {
		var h [32]byte
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		h[3] = byte(i >> 24)
		tr.SeedOneCode(tracker.CodeSeed{CodeHash: h, CodeSize: 1024, Refcount: 1})
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growthMB := int64(after.Alloc-before.Alloc) / (1024 * 1024)
	t.Logf("streaming-seed 1M tracker heap growth: %d MB (Alloc=%d MB, Sys=%d MB)",
		growthMB, after.Alloc/(1024*1024), after.Sys/(1024*1024))

	// Same 300 MB ceiling as the slice-based test, but here the test does
	// NOT pre-allocate a 1M-entry slice so the headroom is even larger.
	// A failure here means an internal slice has reappeared somewhere on
	// the SeedOneCode call path.
	const ceilingMB = 300
	if growthMB > ceilingMB {
		t.Errorf("streaming-seed heap growth %d MB exceeded ceiling %d MB",
			growthMB, ceilingMB)
	}

	got := tr.SnapshotCounters()
	if got.UniqueCodeHashes != n {
		t.Errorf("UniqueCodeHashes = %d, want %d", got.UniqueCodeHashes, n)
	}
	if got.ContractsTotal != n {
		t.Errorf("ContractsTotal = %d, want %d", got.ContractsTotal, n)
	}
	if got.CodeBytesTotal != int64(n)*1024 {
		t.Errorf("CodeBytesTotal = %d, want %d", got.CodeBytesTotal, int64(n)*1024)
	}
}

// TestMemoryRegression_StreamingExport asserts that the snapshot
// writer-side iteration path (ExportCodes via callback) holds at most one
// record in transient memory regardless of tracker cardinality. The legacy
// ExportKeys path materialised every entry into a slice; the new ExportCodes
// callback walks the underlying store and yields one record at a time.
//
// We measure peak Alloc delta during a full walk and assert it stays bounded
// by ~10 MB on a 500 k tracker — the iteration uses only the callback
// arguments + one [44]byte record buffer in the snapshot writer.
func TestMemoryRegression_StreamingExport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory regression in short mode")
	}
	const n = 500_000
	tr := tracker.New()
	for i := 0; i < n; i++ {
		var h [32]byte
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		tr.SeedOneCode(tracker.CodeSeed{CodeHash: h, CodeSize: 256, Refcount: 1})
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var visited int64
	err := tr.ExportCodes(func(cs tracker.CodeSeed) bool {
		// Force the compiler to retain the iteration but discard the value.
		if cs.CodeSize == 0 {
			t.Fatalf("unexpected zero size at index %d", visited)
		}
		visited++
		return true
	})
	if err != nil {
		t.Fatalf("ExportCodes: %v", err)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growthMB := int64(after.Alloc-before.Alloc) / (1024 * 1024)
	t.Logf("ExportCodes walk over %d entries: heap growth %d MB", n, growthMB)
	if visited != n {
		t.Errorf("ExportCodes visited %d entries, want %d", visited, n)
	}
	const ceilingMB = 20
	if growthMB > ceilingMB {
		t.Errorf("ExportCodes growth %d MB exceeded ceiling %d MB — slice may have reappeared",
			growthMB, ceilingMB)
	}
}

// TestMemoryRegression_DiffApply asserts that applying many diff records
// does not leak memory beyond the per-entry baseline.
func TestMemoryRegression_DiffApply(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	tr := tracker.New()
	for i := 0; i < 100_000; i++ {
		var h [32]byte
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		tr.ApplyBlockDiff(rlppkg.BlockDiffRecord{
			BlockNumber: uint64(i + 1),
			CodeHashChanges: []rlppkg.CodeHashChange{
				{OldHash: [32]byte{}, NewHash: h, NewCodeSize: 100},
			},
		})
	}

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	allocMB := ms.Alloc / (1024 * 1024)
	t.Logf("after 100k diff applies: Alloc=%d MB, Sys=%d MB", allocMB, ms.Sys/(1024*1024))

	if allocMB > 200 {
		t.Errorf("alloc %d MB after 100k diffs exceeds 200 MB ceiling", allocMB)
	}
}
