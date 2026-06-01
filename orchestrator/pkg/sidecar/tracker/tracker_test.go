package tracker

import (
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
)

func TestApplyCodeChangeAddsAndRemoves(t *testing.T) {
	tr := New()
	addr := [32]byte{0x01}
	hash := [32]byte{0xab}
	tr.ApplyCodeChange(addr, [32]byte{}, hash, 1024)
	got := tr.SnapshotCounters()
	if got.UniqueCodeHashes != 1 {
		t.Errorf("UniqueCodeHashes = %d, want 1", got.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != 1024 {
		t.Errorf("CodeBytesTotal = %d, want 1024", got.CodeBytesTotal)
	}
	if got.ContractsTotal != 1 {
		t.Errorf("ContractsTotal = %d, want 1", got.ContractsTotal)
	}

	addr2 := [32]byte{0x02}
	tr.ApplyCodeChange(addr2, [32]byte{}, hash, 1024)
	got = tr.SnapshotCounters()
	if got.UniqueCodeHashes != 1 {
		t.Errorf("UniqueCodeHashes after second add = %d, want 1", got.UniqueCodeHashes)
	}
	if got.ContractsTotal != 2 {
		t.Errorf("ContractsTotal after second add = %d, want 2", got.ContractsTotal)
	}
	if got.CodeBytesTotal != 1024 {
		t.Errorf("CodeBytesTotal = %d, want 1024 (deduped)", got.CodeBytesTotal)
	}

	tr.ApplyCodeChange(addr2, hash, [32]byte{}, 0)
	got = tr.SnapshotCounters()
	if got.UniqueCodeHashes != 1 {
		t.Errorf("UniqueCodeHashes after remove = %d, want 1", got.UniqueCodeHashes)
	}
	if got.ContractsTotal != 1 {
		t.Errorf("ContractsTotal after remove = %d, want 1", got.ContractsTotal)
	}

	tr.ApplyCodeChange(addr, hash, [32]byte{}, 0)
	got = tr.SnapshotCounters()
	if got.UniqueCodeHashes != 0 {
		t.Errorf("UniqueCodeHashes after final remove = %d, want 0", got.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != 0 {
		t.Errorf("CodeBytesTotal after final remove = %d, want 0", got.CodeBytesTotal)
	}
}

func TestApplySlotChange(t *testing.T) {
	tr := New()
	addr := [32]byte{0xfe}
	tr.ApplySlotChange(addr, 0, 10)
	got := tr.SnapshotCounters()
	if got.StorageSlotsTotal != 10 {
		t.Errorf("StorageSlotsTotal = %d, want 10", got.StorageSlotsTotal)
	}
	if got.ContractsWithStorage != 1 {
		t.Errorf("ContractsWithStorage = %d, want 1", got.ContractsWithStorage)
	}

	tr.ApplySlotChange(addr, 10, 5)
	got = tr.SnapshotCounters()
	if got.StorageSlotsTotal != 5 {
		t.Errorf("StorageSlotsTotal after dec = %d, want 5", got.StorageSlotsTotal)
	}
	if got.ContractsWithStorage != 1 {
		t.Errorf("ContractsWithStorage stayed 1, got %d", got.ContractsWithStorage)
	}

	tr.ApplySlotChange(addr, 5, 0)
	got = tr.SnapshotCounters()
	if got.StorageSlotsTotal != 0 {
		t.Errorf("StorageSlotsTotal after zero = %d, want 0", got.StorageSlotsTotal)
	}
	if got.ContractsWithStorage != 0 {
		t.Errorf("ContractsWithStorage after zero = %d, want 0", got.ContractsWithStorage)
	}
}

func TestApplyBlockDiffOrdering(t *testing.T) {
	tr := New()
	addr := [32]byte{0x42}
	hash1 := [32]byte{0xa1}
	hash2 := [32]byte{0xa2}

	tr.ApplyBlockDiff(rlp.BlockDiffRecord{
		BlockNumber: 100,
		CodeHashChanges: []rlp.CodeHashChange{
			{OldHash: [32]byte{}, NewHash: hash1, NewCodeSize: 200},
		},
	})
	tr.ApplyBlockDiff(rlp.BlockDiffRecord{
		BlockNumber: 101,
		CodeHashChanges: []rlp.CodeHashChange{
			{OldHash: hash1, NewHash: hash2, NewCodeSize: 300},
		},
		SlotCountChanges: []rlp.SlotCountChange{
			{HashedAddress: addr, OldCount: 0, NewCount: 4},
		},
	})

	got := tr.SnapshotCounters()
	if tr.LastBlock() != 101 {
		t.Errorf("LastBlock = %d, want 101", tr.LastBlock())
	}
	if got.UniqueCodeHashes != 1 {
		t.Errorf("UniqueCodeHashes = %d, want 1 (only hash2 alive)", got.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != 300 {
		t.Errorf("CodeBytesTotal = %d, want 300", got.CodeBytesTotal)
	}
	if got.StorageSlotsTotal != 4 {
		t.Errorf("StorageSlotsTotal = %d, want 4", got.StorageSlotsTotal)
	}
}

func TestSeedFromScan(t *testing.T) {
	tr := New()
	tr.SeedFromScan(
		[]CodeSeed{
			{CodeHash: [32]byte{0x01}, CodeSize: 100, Refcount: 3},
			{CodeHash: [32]byte{0x02}, CodeSize: 200, Refcount: 1},
		},
		[]SlotSeed{
			{HashedAddress: [32]byte{0xaa}, SlotCount: 50},
			{HashedAddress: [32]byte{0xbb}, SlotCount: 0}, // skipped
		},
	)
	got := tr.SnapshotCounters()
	if got.UniqueCodeHashes != 2 {
		t.Errorf("UniqueCodeHashes = %d, want 2", got.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != 300 {
		t.Errorf("CodeBytesTotal = %d, want 300", got.CodeBytesTotal)
	}
	if got.ContractsTotal != 4 {
		t.Errorf("ContractsTotal = %d, want 4 (sum of refcounts)", got.ContractsTotal)
	}
	if got.StorageSlotsTotal != 50 {
		t.Errorf("StorageSlotsTotal = %d, want 50", got.StorageSlotsTotal)
	}
	if got.ContractsWithStorage != 1 {
		t.Errorf("ContractsWithStorage = %d, want 1", got.ContractsWithStorage)
	}
}

// TestSeedOneCodeMatchesSeedFromScan asserts one-at-a-time SeedOneCode produces
// the same counters as SeedFromScan, guaranteeing the streaming path is correct.
func TestSeedOneCodeMatchesSeedFromScan(t *testing.T) {
	seeds := []CodeSeed{
		{CodeHash: [32]byte{0x10}, CodeSize: 100, Refcount: 1},
		{CodeHash: [32]byte{0x20}, CodeSize: 200, Refcount: 2},
		{CodeHash: [32]byte{0x30}, CodeSize: 300, Refcount: 3},
		{CodeHash: [32]byte{0x40}, CodeSize: 400, Refcount: 4},
	}

	a := New()
	a.SeedFromScan(seeds, nil)

	b := New()
	for _, s := range seeds {
		b.SeedOneCode(s)
	}

	got := a.SnapshotCounters()
	want := b.SnapshotCounters()
	if got.UniqueCodeHashes != want.UniqueCodeHashes {
		t.Errorf("UniqueCodeHashes: bulk=%d streaming=%d", got.UniqueCodeHashes, want.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != want.CodeBytesTotal {
		t.Errorf("CodeBytesTotal: bulk=%d streaming=%d", got.CodeBytesTotal, want.CodeBytesTotal)
	}
	if got.ContractsTotal != want.ContractsTotal {
		t.Errorf("ContractsTotal: bulk=%d streaming=%d", got.ContractsTotal, want.ContractsTotal)
	}
}

// TestRebuildCountersFromCodeStore guards the CodesExternal restart path:
// snapshot.bin carries no code records; counters must rebuild from the store alone.
func TestRebuildCountersFromCodeStore(t *testing.T) {
	a := New()
	a.SeedOneCode(CodeSeed{CodeHash: [32]byte{0xa1}, CodeSize: 500, Refcount: 5})
	a.SeedOneCode(CodeSeed{CodeHash: [32]byte{0xa2}, CodeSize: 700, Refcount: 7})

	store := a.CodeStore()
	b := NewWithCodeStore(store)
	if err := b.RebuildCountersFromCodeStore(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	got := b.SnapshotCounters()
	want := a.SnapshotCounters()
	if got.UniqueCodeHashes != want.UniqueCodeHashes {
		t.Errorf("UniqueCodeHashes: rebuilt=%d want=%d", got.UniqueCodeHashes, want.UniqueCodeHashes)
	}
	if got.CodeBytesTotal != want.CodeBytesTotal {
		t.Errorf("CodeBytesTotal: rebuilt=%d want=%d", got.CodeBytesTotal, want.CodeBytesTotal)
	}
	if got.ContractsTotal != want.ContractsTotal {
		t.Errorf("ContractsTotal: rebuilt=%d want=%d", got.ContractsTotal, want.ContractsTotal)
	}
}

// TestStreamingSeed1M_Correctness verifies streaming-seed counter accuracy at
// 1M entries (the scale that OOMs the slice-based path).
func TestStreamingSeed1M_Correctness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1M streaming-seed correctness test in short mode")
	}
	const n = 1_000_000
	tr := New()
	var expectedBytes int64
	for i := 0; i < n; i++ {
		var h [32]byte
		h[0] = byte(i)
		h[1] = byte(i >> 8)
		h[2] = byte(i >> 16)
		h[3] = byte(i >> 24)
		size := uint64(100 + i%512)
		tr.SeedOneCode(CodeSeed{CodeHash: h, CodeSize: size, Refcount: 1})
		expectedBytes += int64(size)
	}
	got := tr.SnapshotCounters()
	if got.UniqueCodeHashes != n {
		t.Errorf("UniqueCodeHashes = %d, want %d", got.UniqueCodeHashes, n)
	}
	if got.ContractsTotal != n {
		t.Errorf("ContractsTotal = %d, want %d", got.ContractsTotal, n)
	}
	if got.CodeBytesTotal != expectedBytes {
		t.Errorf("CodeBytesTotal = %d, want %d", got.CodeBytesTotal, expectedBytes)
	}
}

func TestAdvanceLastBlockMonotonic(t *testing.T) {
	tr := New()

	tr.AdvanceLastBlock(100)
	if got := tr.LastBlock(); got != 100 {
		t.Fatalf("after Advance(100): LastBlock=%d, want 100", got)
	}

	tr.AdvanceLastBlock(50)
	if got := tr.LastBlock(); got != 100 {
		t.Fatalf("after Advance(50): LastBlock=%d, want 100 (monotonic)", got)
	}

	tr.AdvanceLastBlock(150)
	if got := tr.LastBlock(); got != 150 {
		t.Fatalf("after Advance(150): LastBlock=%d, want 150", got)
	}

	root := [32]byte{0xab}
	tr.SetLastBlock(200, root)
	tr.AdvanceLastBlock(250)
	if tr.StateRoot() != root {
		t.Fatalf("AdvanceLastBlock cleared the state root; want pin %x, got %x", root, tr.StateRoot())
	}
}
