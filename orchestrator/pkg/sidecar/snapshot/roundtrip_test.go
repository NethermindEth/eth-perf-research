package snapshot

import (
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tr := tracker.New()
	tr.SetScanCounters(tracker.ScanCounters{
		BlockNumber:           42,
		StateRoot:             [32]byte{0xaa, 0xbb, 0xcc},
		AccountsTotal:         1_000_000,
		EmptyAccounts:         123,
		AccountTrieBranches:   55,
		AccountTrieExtensions: 66,
		AccountTrieLeaves:     1_000_000,
		AccountTrieBytes:      1234,
		StorageTrieBranches:   77,
		StorageTrieExtensions: 88,
		StorageTrieLeaves:     99,
		StorageTrieBytes:      4321,
		SlotHistogram:         []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	})
	tr.SeedFromScan(
		[]tracker.CodeSeed{
			{CodeHash: [32]byte{0x01}, CodeSize: 100, Refcount: 2},
			{CodeHash: [32]byte{0x02}, CodeSize: 200, Refcount: 1},
		},
		[]tracker.SlotSeed{
			{HashedAddress: [32]byte{0xaa}, SlotCount: 5},
			{HashedAddress: [32]byte{0xbb}, SlotCount: 50},
		},
	)
	tr.SetLastBlock(42, [32]byte{0xaa, 0xbb, 0xcc})

	if _, err := Write(dir, tr); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, hdr, err := Restore(dir)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if hdr.BlockNumber != 42 {
		t.Errorf("hdr.BlockNumber = %d, want 42", hdr.BlockNumber)
	}
	if hdr.AccountsTotal != 1_000_000 {
		t.Errorf("hdr.AccountsTotal = %d, want 1_000_000", hdr.AccountsTotal)
	}
	rec := got.SnapshotCounters()
	if rec.AccountsTotal != 1_000_000 {
		t.Errorf("restored AccountsTotal = %d, want 1_000_000", rec.AccountsTotal)
	}
	if rec.CodeBytesTotal != 300 {
		t.Errorf("restored CodeBytesTotal = %d, want 300", rec.CodeBytesTotal)
	}
	if rec.UniqueCodeHashes != 2 {
		t.Errorf("restored UniqueCodeHashes = %d, want 2", rec.UniqueCodeHashes)
	}
	if rec.ContractsTotal != 3 {
		t.Errorf("restored ContractsTotal = %d, want 3", rec.ContractsTotal)
	}
	if rec.StorageSlotsTotal != 55 {
		t.Errorf("restored StorageSlotsTotal = %d, want 55", rec.StorageSlotsTotal)
	}
	if rec.AccountTrieBytes != 1234 {
		t.Errorf("restored AccountTrieBytes = %d, want 1234", rec.AccountTrieBytes)
	}
	if rec.SlotHistogram[15] != 16 {
		t.Errorf("restored SlotHistogram[15] = %d, want 16", rec.SlotHistogram[15])
	}
	if got.LastBlock() != 42 {
		t.Errorf("LastBlock = %d, want 42", got.LastBlock())
	}
}

func TestRestoreMissing(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Restore(dir); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	tr := tracker.New()
	tr.SetScanCounters(tracker.ScanCounters{BlockNumber: 1, AccountsTotal: 7})
	if _, err := Write(dir, tr); err != nil {
		t.Fatalf("first write: %v", err)
	}

	tr2 := tracker.New()
	tr2.SetScanCounters(tracker.ScanCounters{BlockNumber: 2, AccountsTotal: 9})
	if _, err := Write(dir, tr2); err != nil {
		t.Fatalf("second write: %v", err)
	}
	hdr, err := ReadHeader(dir)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if hdr.AccountsTotal != 9 {
		t.Errorf("expected second-write value 9, got %d", hdr.AccountsTotal)
	}
}
