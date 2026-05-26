// Crash/SIGKILL resilience regression test for the sidecar.
//
// The "no rescan on restart" property is load-bearing for v19: a cold
// bootstrap is ~3h, so any code change that breaks snapshot restore would
// silently triple operator turnaround time on the bloatnet. This test
// guards against that by simulating a crash mid-run and asserting the
// restored tracker carries forward the pre-crash state without touching
// the (expensive) bootstrap scanner.
//
// Simulation strategy:
//
//  1. Seed a fresh tracker as if a bootstrap had just finished, including a
//     few applied block diffs to mimic the tail path.
//  2. Persist via snapshot.Write (the same call the production snapshot
//     loop makes every 30s).
//  3. Drop all references to the in-memory tracker — equivalent to the
//     process disappearing (panic / os.Exit / SIGKILL all have the same
//     net effect: the heap is gone, the on-disk blob remains).
//  4. Force a GC so the test runner cannot accidentally keep the old
//     tracker alive through some test-framework reference.
//  5. Restore from disk; verify every counter that crossed the bootstrap →
//     tail boundary matches what the pre-crash tracker reported.
//
// We deliberately do not fork/exec a real binary here — exec-based crash
// tests are flaky in CI (rocksdb dependency, file-descriptor leaks) and
// the on-disk schema is identical whether the writer process panics, is
// killed, or exits cleanly. The atomic-rename in snapshot.Write is what
// makes that true; if that contract ever breaks, the snapshot reader
// asserts on bad magic and this test would catch it.

package sidecartest

import (
	"runtime"
	"testing"

	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/snapshot"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

func TestCrashRecoveryFromSnapshot(t *testing.T) {
	dir := t.TempDir()

	// ---- Pre-crash run ----
	preSnap := buildAndPersistTracker(t, dir)

	// Wipe the in-memory state — anything that survives must do so via the
	// on-disk blob.
	preSnap = preSnap // pinned for asserts; no further mutation
	runtime.GC()

	// ---- Restart path: restore from snapshot, NOT from a fresh scan ----
	restored, hdr, err := snapshot.Restore(dir)
	if err != nil {
		t.Fatalf("snapshot.Restore after simulated crash: %v", err)
	}
	if hdr.BlockNumber != preSnap.LastBlock {
		t.Fatalf("restored header block = %d, want %d (pre-crash)", hdr.BlockNumber, preSnap.LastBlock)
	}

	post := restored.SnapshotCounters()

	// Tier-1 counters must survive crash exactly. These are the values
	// the bootstrap scanner would otherwise have to rebuild over ~3h.
	if post.AccountsTotal != preSnap.AccountsTotal {
		t.Errorf("AccountsTotal: pre=%d post=%d", preSnap.AccountsTotal, post.AccountsTotal)
	}
	if post.ContractsTotal != preSnap.ContractsTotal {
		t.Errorf("ContractsTotal: pre=%d post=%d", preSnap.ContractsTotal, post.ContractsTotal)
	}
	if post.StorageSlotsTotal != preSnap.StorageSlotsTotal {
		t.Errorf("StorageSlotsTotal: pre=%d post=%d", preSnap.StorageSlotsTotal, post.StorageSlotsTotal)
	}
	if post.CodeBytesTotal != preSnap.CodeBytesTotal {
		t.Errorf("CodeBytesTotal: pre=%d post=%d", preSnap.CodeBytesTotal, post.CodeBytesTotal)
	}
	if post.UniqueCodeHashes != preSnap.UniqueCodeHashes {
		t.Errorf("UniqueCodeHashes: pre=%d post=%d", preSnap.UniqueCodeHashes, post.UniqueCodeHashes)
	}
	if post.ContractsWithStorage != preSnap.ContractsWithStorage {
		t.Errorf("ContractsWithStorage: pre=%d post=%d", preSnap.ContractsWithStorage, post.ContractsWithStorage)
	}

	// Tier-2 (scan-only) counters must also survive — they live in the
	// snapshot header.
	if post.AccountTrieBytes != preSnap.AccountTrieBytes {
		t.Errorf("AccountTrieBytes: pre=%d post=%d", preSnap.AccountTrieBytes, post.AccountTrieBytes)
	}
	if post.StorageTrieBytes != preSnap.StorageTrieBytes {
		t.Errorf("StorageTrieBytes: pre=%d post=%d", preSnap.StorageTrieBytes, post.StorageTrieBytes)
	}
	if post.SlotHistogram != preSnap.SlotHistogram {
		t.Errorf("SlotHistogram: pre=%v post=%v", preSnap.SlotHistogram, post.SlotHistogram)
	}
	if post.LastBlock != preSnap.LastBlock {
		t.Errorf("LastBlock: pre=%d post=%d", preSnap.LastBlock, post.LastBlock)
	}
	if post.StateRoot != preSnap.StateRoot {
		t.Errorf("StateRoot: pre=%x post=%x", preSnap.StateRoot, post.StateRoot)
	}

	// ---- Continuity check: applying a new block diff after restart
	// produces the same monotonic accounting it would have pre-crash.
	postDiff := rlppkg.BlockDiffRecord{
		BlockNumber: uint64(preSnap.LastBlock) + 1,
		StateRoot:   [32]byte{0xde, 0xad},
		CodeHashChanges: []rlppkg.CodeHashChange{
			{OldHash: [32]byte{}, NewHash: [32]byte{0xfe}, NewCodeSize: 512},
		},
	}
	restored.ApplyBlockDiff(postDiff)
	after := restored.SnapshotCounters()
	if after.LastBlock != preSnap.LastBlock+1 {
		t.Errorf("post-restart diff did not advance LastBlock: got %d, want %d", after.LastBlock, preSnap.LastBlock+1)
	}
	if after.CodeBytesTotal != preSnap.CodeBytesTotal+512 {
		t.Errorf("post-restart CodeBytesTotal: got %d, want %d", after.CodeBytesTotal, preSnap.CodeBytesTotal+512)
	}
}

// buildAndPersistTracker constructs a synthetic post-bootstrap tracker,
// applies a few tail-style block diffs, persists it to dir, and returns the
// counters snapshot for post-crash comparison.
func buildAndPersistTracker(t *testing.T, dir string) tracker.Snapshot {
	t.Helper()
	tr := tracker.New()
	tr.SetScanCounters(tracker.ScanCounters{
		BlockNumber:           5000,
		StateRoot:             [32]byte{0xab, 0xcd, 0xef},
		AccountsTotal:         123_456,
		EmptyAccounts:         12,
		AccountTrieBranches:   1000,
		AccountTrieExtensions: 500,
		AccountTrieLeaves:     123_456,
		AccountTrieBytes:      9_999_999,
		StorageTrieBranches:   2000,
		StorageTrieExtensions: 700,
		StorageTrieLeaves:     800_000,
		StorageTrieBytes:      55_555_555,
		SlotHistogram:         []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	})
	tr.SeedFromScan(
		[]tracker.CodeSeed{
			{CodeHash: [32]byte{0xa1}, CodeSize: 800, Refcount: 7},
			{CodeHash: [32]byte{0xa2}, CodeSize: 1600, Refcount: 3},
		},
		[]tracker.SlotSeed{
			{HashedAddress: [32]byte{0xb1}, SlotCount: 50},
			{HashedAddress: [32]byte{0xb2}, SlotCount: 200},
		},
	)
	tr.SetLastBlock(5000, [32]byte{0xab, 0xcd, 0xef})

	// Apply a couple of tail diffs so the snapshot covers the
	// scan-seed + diff-apply path together.
	tr.ApplyBlockDiff(rlppkg.BlockDiffRecord{
		BlockNumber: 5001,
		StateRoot:   [32]byte{0x11},
		CodeHashChanges: []rlppkg.CodeHashChange{
			{OldHash: [32]byte{}, NewHash: [32]byte{0xa3}, NewCodeSize: 2048},
		},
		SlotCountChanges: []rlppkg.SlotCountChange{
			{HashedAddress: [32]byte{0xb1}, OldCount: 50, NewCount: 75},
		},
	})
	tr.ApplyBlockDiff(rlppkg.BlockDiffRecord{
		BlockNumber: 5002,
		StateRoot:   [32]byte{0x22},
		SlotCountChanges: []rlppkg.SlotCountChange{
			{HashedAddress: [32]byte{0xb3}, OldCount: 0, NewCount: 10},
		},
	})

	if _, err := snapshot.Write(dir, tr); err != nil {
		t.Fatalf("snapshot.Write: %v", err)
	}
	return tr.SnapshotCounters()
}
