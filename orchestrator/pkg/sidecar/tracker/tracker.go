package tracker

import (
	"sync/atomic"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
)

// Tracker is the sidecar's authoritative state composition counter store.
//
// Responsibilities:
//   - hold per-codeHash refcounts and per-contract slot counts.
//   - maintain the aggregate counters (uniqueCodeHashes, codeBytesTotal,
//     storageSlotsTotal, contractsTotal, contractsWithStorage).
//   - expose them to the RPC server with O(1) reads.
//
// Tier-2 scan-only counters (accountTrieBytes, slotCountHistogram, etc.) are
// owned by the bootstrap scanner and carried forward across diffs by the
// state holder. The tracker proper only owns the deltas the diff-stream
// touches.
//
// Code data is stored in a pluggable CodeStore (see codestore.go). Default is
// the in-RAM map; bloatnet runs swap it for the on-disk RocksDB-backed store
// via UseCodeStore so 1.4 B+ codehash entries never need to fit in memory.
type Tracker struct {
	shards [ShardCount]*shard

	// codes is the CodeStore shared by every shard. Owned by the tracker
	// (it manages Close); set by New (memCodeStore default) or replaced by
	// UseCodeStore before any diff applies.
	codes CodeStore

	// Scan-only counters (set by the bootstrap scanner once, surfaced
	// verbatim by the RPC). Stored as atomics so /lite can read them
	// without grabbing every shard lock.
	accountsTotal         atomic.Int64
	emptyAccounts         atomic.Int64
	accountTrieBranches   atomic.Int64
	accountTrieExtensions atomic.Int64
	accountTrieLeaves     atomic.Int64
	accountTrieBytes      atomic.Int64
	storageTrieBranches   atomic.Int64
	storageTrieExtensions atomic.Int64
	storageTrieLeaves     atomic.Int64
	storageTrieBytes      atomic.Int64

	// Calibration ratios captured at bootstrap end: bytes-per-account-trie-
	// record and bytes-per-storage-slot. Used by ApplyBlockDiff to estimate
	// tier-2 byte deltas from the diff stream so accountTrieBytes/
	// storageTrieBytes stay live between full re-scans. Set by SetScanCounters.
	bytesPerAccount atomic.Int64
	bytesPerSlot    atomic.Int64

	// Slot histogram (length CumulativeTrieStats.SlotHistogramLength == 16).
	slotHistogram [SlotHistogramLength]atomic.Int64

	// Last block number this tracker has folded in. Bootstrap → scan block;
	// each tail apply bumps it.
	lastBlock atomic.Int64
	stateRoot atomic.Value // [32]byte (stored as value to avoid lock)
}

// SlotHistogramLength must match Nethermind.StateComposition.Data.CumulativeTrieStats.SlotHistogramLength.
const SlotHistogramLength = 16

// New constructs an empty tracker with all shards initialised and an in-memory
// CodeStore as default. Production code paths that need to scale beyond RAM
// should call UseCodeStore with a disk-backed implementation before seeding.
func New() *Tracker {
	return NewWithCodeStore(newMemCodeStore())
}

// NewWithCodeStore is like New but installs the supplied CodeStore as the
// per-codehash backing. Used by the bootstrap path to wire in the on-disk
// store before SeedCodesStreaming runs.
func NewWithCodeStore(codes CodeStore) *Tracker {
	t := &Tracker{codes: codes}
	for i := 0; i < ShardCount; i++ {
		t.shards[i] = newShard(codes)
	}
	t.stateRoot.Store([32]byte{})
	return t
}

// UseCodeStore swaps the active CodeStore. The previous store is NOT closed
// — the caller owns its lifetime. Must be called before any diffs are
// applied, otherwise existing in-memory counters will not reflect the
// re-pointed store.
func (t *Tracker) UseCodeStore(c CodeStore) {
	t.codes = c
	for i := 0; i < ShardCount; i++ {
		t.shards[i].codes = c
	}
}

// CodeStore exposes the active code-store implementation for callers that
// need direct iteration (e.g. snapshot writers).
func (t *Tracker) CodeStore() CodeStore { return t.codes }

// CodesAreExternal reports whether the active CodeStore is disk-backed
// (i.e. survives process restart on its own). When true the snapshot writer
// may skip serialising per-codehash records because the cold tier is the
// authoritative source. The in-memory store returns false because its
// contents are lost when the process exits.
func (t *Tracker) CodesAreExternal() bool {
	_, isMem := t.codes.(*memCodeStore)
	return t.codes != nil && !isMem
}

// ApplyCodeChange routes a CodeHashChange to its owning shards. Old and new
// hashes are routed independently because they may live in different shards.
// Refcount lookups are keyed by codeHash, not by address.
func (t *Tracker) ApplyCodeChange(addr, oldHash, newHash [32]byte, newSize uint64) {
	if oldHash != zeroHash {
		t.shards[shardOf(oldHash)].applyCodeRemove(oldHash)
	}
	if newHash != zeroHash {
		t.shards[shardOf(newHash)].applyCodeAdd(newHash, newSize)
	}
}

// ApplySlotChange routes a SlotCountChange to its owning shard.
func (t *Tracker) ApplySlotChange(addr [32]byte, oldCount, newCount uint64) {
	idx := shardOf(addr)
	t.shards[idx].applySlotChange(addr, oldCount, newCount)
}

// ApplyBlockDiff folds all changes from one BlockDiffRecord.
//
// Tier-2 byte totals come from one of two sources, in priority order:
//   - Explicit deltas on the record (NM plugin PR-A v2 wire format) when
//     any of AccountTrieBytesDelta / StorageTrieBytesDelta / AccountsAddedDelta
//     is non-zero. Bit-exact, captured by the trie diff walker on the NM side.
//   - Fallback estimator using the calibration ratios (bytesPerAccount /
//     bytesPerSlot) captured at bootstrap. Used when the NM plugin still
//     emits the legacy schema-v1 record (all three trailing fields = 0).
func (t *Tracker) ApplyBlockDiff(rec rlp.BlockDiffRecord) {
	var slotDelta int64
	var accountAddedDelta int64
	for _, c := range rec.CodeHashChanges {
		t.ApplyCodeChange(zeroHash, c.OldHash, c.NewHash, c.NewCodeSize)
		if c.OldHash == zeroHash && c.NewHash != zeroHash {
			accountAddedDelta++
		}
	}
	for _, s := range rec.SlotCountChanges {
		t.ApplySlotChange(s.HashedAddress, s.OldCount, s.NewCount)
		slotDelta += int64(s.NewCount) - int64(s.OldCount)
		if s.OldCount == 0 && s.NewCount > 0 {
			accountAddedDelta++
		}
	}

	hasExplicit := rec.AccountTrieBytesDelta != 0 ||
		rec.StorageTrieBytesDelta != 0 ||
		rec.AccountsAddedDelta != 0
	if hasExplicit {
		if rec.AccountTrieBytesDelta != 0 {
			t.accountTrieBytes.Add(rec.AccountTrieBytesDelta)
		}
		if rec.StorageTrieBytesDelta != 0 {
			t.storageTrieBytes.Add(rec.StorageTrieBytesDelta)
		}
		if rec.AccountsAddedDelta != 0 {
			t.accountsTotal.Add(rec.AccountsAddedDelta)
		}
	} else {
		if bps := t.bytesPerSlot.Load(); bps != 0 && slotDelta != 0 {
			t.storageTrieBytes.Add(slotDelta * bps)
		}
		if bpa := t.bytesPerAccount.Load(); bpa != 0 && accountAddedDelta != 0 {
			t.accountTrieBytes.Add(accountAddedDelta * bpa)
			t.accountsTotal.Add(accountAddedDelta)
		}
	}
	if int64(rec.BlockNumber) > t.lastBlock.Load() {
		t.lastBlock.Store(int64(rec.BlockNumber))
		t.stateRoot.Store(rec.StateRoot)
	}
}

// SetScanCounters is called once by the bootstrap scanner to seed the tier-2
// (depth/byte) values the RPC carries verbatim.
func (t *Tracker) SetScanCounters(c ScanCounters) {
	t.accountsTotal.Store(c.AccountsTotal)
	t.emptyAccounts.Store(c.EmptyAccounts)
	t.accountTrieBranches.Store(c.AccountTrieBranches)
	t.accountTrieExtensions.Store(c.AccountTrieExtensions)
	t.accountTrieLeaves.Store(c.AccountTrieLeaves)
	t.accountTrieBytes.Store(c.AccountTrieBytes)
	t.storageTrieBranches.Store(c.StorageTrieBranches)
	t.storageTrieExtensions.Store(c.StorageTrieExtensions)
	t.storageTrieLeaves.Store(c.StorageTrieLeaves)
	t.storageTrieBytes.Store(c.StorageTrieBytes)
	// Calibration: bytes-per-record ratios so ApplyBlockDiff can estimate
	// tier-2 byte deltas without an NM-side plumbing change. Bootstrap
	// produces accurate ratios; bloating may drift them (e.g. fresh EOAs
	// have minimal storage), but over short windows the estimate is good
	// enough for the controller's gradient.
	if c.AccountsTotal > 0 {
		t.bytesPerAccount.Store(c.AccountTrieBytes / c.AccountsTotal)
	}
	var slotsTotal int64
	for i := 0; i < ShardCount; i++ {
		slotsTotal += t.shards[i].storageSlotsTotal
	}
	if slotsTotal > 0 {
		t.bytesPerSlot.Store(c.StorageTrieBytes / slotsTotal)
	}
	for i := 0; i < SlotHistogramLength && i < len(c.SlotHistogram); i++ {
		t.slotHistogram[i].Store(c.SlotHistogram[i])
	}
	if c.BlockNumber > t.lastBlock.Load() {
		t.lastBlock.Store(c.BlockNumber)
		t.stateRoot.Store(c.StateRoot)
	}
}

// ScanCounters is the seed payload the bootstrap scanner hands to the tracker.
type ScanCounters struct {
	BlockNumber           int64
	StateRoot             [32]byte
	AccountsTotal         int64
	EmptyAccounts         int64
	AccountTrieBranches   int64
	AccountTrieExtensions int64
	AccountTrieLeaves     int64
	AccountTrieBytes      int64
	StorageTrieBranches   int64
	StorageTrieExtensions int64
	StorageTrieLeaves     int64
	StorageTrieBytes      int64
	SlotHistogram         []int64

	// Also seeds the per-codeHash refcounts so contractsTotal /
	// codeBytesTotal / uniqueCodeHashes are non-zero before any diffs flow.
	CodeSeed []CodeSeed
	SlotSeed []SlotSeed
}

// CodeSeed represents one row of the bootstrap scanner's code-hash output.
type CodeSeed struct {
	CodeHash [32]byte
	CodeSize uint64
	Refcount uint32
}

// SlotSeed represents one row of the bootstrap scanner's per-contract slot
// count output.
type SlotSeed struct {
	HashedAddress [32]byte
	SlotCount     uint64
}

// SeedFromScan applies the per-key seeds discovered during bootstrap. Must be
// called before the tracker is exposed to the tailer.
//
// This is the in-RAM seed path used by tests and small deployments. At
// bloatnet scale (1.4 B+ codehashes) callers must instead use
// SeedCodesStreaming + SeedSlotsStreaming so the per-key data lands in the
// (disk-backed) CodeStore without first materialising as a Go slice.
func (t *Tracker) SeedFromScan(codes []CodeSeed, slots []SlotSeed) {
	for _, cs := range codes {
		t.SeedOneCode(cs)
	}
	for _, ss := range slots {
		t.SeedOneSlot(ss)
	}
}

// SeedOneCode applies a single CodeSeed to its owning shard and the active
// CodeStore. Equivalent to SeedFromScan with a one-element slice. Exposed so
// the bootstrap merge-join can stream records straight into the tracker
// without ever materialising a []CodeSeed in memory.
func (t *Tracker) SeedOneCode(cs CodeSeed) {
	idx := shardOf(cs.CodeHash)
	sh := t.shards[idx]
	sh.mu.Lock()
	sh.codes.Put(cs.CodeHash, codeEntry{Refcount: cs.Refcount, CodeSize: cs.CodeSize})
	sh.uniqueCodeHashes++
	sh.codeBytesTotal += int64(cs.CodeSize)
	sh.contractsTotal += int64(cs.Refcount)
	sh.mu.Unlock()
}

// ShardCodeCounter is the per-shard subset of code-related counters needed
// to skip RebuildCountersFromCodeStore on restart. Storage-side counters
// (storageSlotsTotal, contractsWithStorage, slot map) re-populate naturally
// via SeedOneSlot replay from snapshot.bin, so they are not carried here.
type ShardCodeCounter struct {
	UniqueCodeHashes int64
	CodeBytesTotal   int64
	ContractsTotal   int64
}

// ExportShardCodeCounters returns one ShardCodeCounter per shard, in shard
// index order. The slice is freshly allocated; modifying it does not touch
// tracker state. Read under per-shard RLock to ensure each shard's three
// counters are mutually consistent.
func (t *Tracker) ExportShardCodeCounters() []ShardCodeCounter {
	out := make([]ShardCodeCounter, ShardCount)
	for i := 0; i < ShardCount; i++ {
		sh := t.shards[i]
		sh.mu.RLock()
		out[i] = ShardCodeCounter{
			UniqueCodeHashes: sh.uniqueCodeHashes,
			CodeBytesTotal:   sh.codeBytesTotal,
			ContractsTotal:   sh.contractsTotal,
		}
		sh.mu.RUnlock()
	}
	return out
}

// SetShardCodeCounters installs per-shard code counters without walking the
// CodeStore. Used by snapshot.RestoreInto when the snapshot carries them
// (schema v2+), eliminating the multi-minute RebuildCountersFromCodeStore
// pass at restart. Length must equal ShardCount; mismatched input is a
// no-op so a corrupted/legacy snapshot can fall back to the rebuild path.
func (t *Tracker) SetShardCodeCounters(c []ShardCodeCounter) bool {
	if len(c) != ShardCount {
		return false
	}
	for i := 0; i < ShardCount; i++ {
		sh := t.shards[i]
		sh.mu.Lock()
		sh.uniqueCodeHashes = c[i].UniqueCodeHashes
		sh.codeBytesTotal = c[i].CodeBytesTotal
		sh.contractsTotal = c[i].ContractsTotal
		sh.mu.Unlock()
	}
	return true
}

// RebuildCountersFromCodeStore walks the active CodeStore and rebuilds the
// per-shard aggregate code counters (uniqueCodeHashes, codeBytesTotal,
// contractsTotal) without touching the store. Used by snapshot.RestoreInto
// when the writer marked codes as external — the codestore is already on
// disk, but the in-memory counters in the new tracker start at zero and
// need to be reconciled against it.
//
// Callers must ensure no diffs are being applied concurrently; we take
// every shard's mu in turn to keep the per-hash sums consistent.
func (t *Tracker) RebuildCountersFromCodeStore() error {
	if t.codes == nil {
		return nil
	}
	return t.codes.Iterate(func(h [32]byte, e codeEntry) bool {
		idx := shardOf(h)
		sh := t.shards[idx]
		sh.mu.Lock()
		sh.uniqueCodeHashes++
		sh.codeBytesTotal += int64(e.CodeSize)
		sh.contractsTotal += int64(e.Refcount)
		sh.mu.Unlock()
		return true
	})
}

// SeedOneSlot applies a single SlotSeed to its owning shard. Records with
// SlotCount == 0 are skipped (no on-disk presence required).
func (t *Tracker) SeedOneSlot(ss SlotSeed) {
	if ss.SlotCount == 0 {
		return
	}
	idx := shardOf(ss.HashedAddress)
	sh := t.shards[idx]
	sh.mu.Lock()
	sh.slots[ss.HashedAddress] = ss.SlotCount
	sh.storageSlotsTotal += int64(ss.SlotCount)
	sh.contractsWithStorage++
	sh.mu.Unlock()
}

// Snapshot captures a coherent point-in-time view of the tier-1 counters.
type Snapshot struct {
	LastBlock            int64
	StateRoot            [32]byte
	AccountsTotal        int64
	ContractsTotal       int64
	StorageSlotsTotal    int64
	CodeBytesTotal       int64
	UniqueCodeHashes     int64
	ContractsWithStorage int64
	EmptyAccounts        int64

	AccountTrieBranches   int64
	AccountTrieExtensions int64
	AccountTrieLeaves     int64
	AccountTrieBytes      int64
	StorageTrieBranches   int64
	StorageTrieExtensions int64
	StorageTrieLeaves     int64
	StorageTrieBytes      int64

	SlotHistogram [SlotHistogramLength]int64
}

// SnapshotCounters returns the merged counters across all shards plus the
// scanner-seeded tier-2 values. O(ShardCount) — never walks any per-key map.
func (t *Tracker) SnapshotCounters() Snapshot {
	var agg shardCounters
	for i := 0; i < ShardCount; i++ {
		agg = agg.add(t.shards[i].snapshotCounters())
	}
	root, _ := t.stateRoot.Load().([32]byte)
	out := Snapshot{
		LastBlock:             t.lastBlock.Load(),
		StateRoot:             root,
		AccountsTotal:         t.accountsTotal.Load(),
		ContractsTotal:        agg.ContractsTotal,
		StorageSlotsTotal:     agg.StorageSlotsTotal,
		CodeBytesTotal:        agg.CodeBytesTotal,
		UniqueCodeHashes:      agg.UniqueCodeHashes,
		ContractsWithStorage:  agg.ContractsWithStorage,
		EmptyAccounts:         t.emptyAccounts.Load(),
		AccountTrieBranches:   t.accountTrieBranches.Load(),
		AccountTrieExtensions: t.accountTrieExtensions.Load(),
		AccountTrieLeaves:     t.accountTrieLeaves.Load(),
		AccountTrieBytes:      t.accountTrieBytes.Load(),
		StorageTrieBranches:   t.storageTrieBranches.Load(),
		StorageTrieExtensions: t.storageTrieExtensions.Load(),
		StorageTrieLeaves:     t.storageTrieLeaves.Load(),
		StorageTrieBytes:      t.storageTrieBytes.Load(),
	}
	for i := 0; i < SlotHistogramLength; i++ {
		out.SlotHistogram[i] = t.slotHistogram[i].Load()
	}
	return out
}

// LastBlock returns the highest block number folded into the tracker.
func (t *Tracker) LastBlock() int64 { return t.lastBlock.Load() }

// StateRoot returns the state root associated with LastBlock.
func (t *Tracker) StateRoot() [32]byte {
	r, _ := t.stateRoot.Load().([32]byte)
	return r
}

// SetLastBlock seeds the lastBlock/stateRoot pair without touching counters.
// Used by snapshot.Restore.
func (t *Tracker) SetLastBlock(block int64, root [32]byte) {
	t.lastBlock.Store(block)
	t.stateRoot.Store(root)
}

// ExportCodes streams every code-hash entry through fn. Returning false from
// fn aborts the walk. Memory: O(1) per call.
func (t *Tracker) ExportCodes(fn func(CodeSeed) bool) error {
	if t.codes == nil {
		return nil
	}
	return t.codes.Iterate(func(h [32]byte, e codeEntry) bool {
		return fn(CodeSeed{CodeHash: h, CodeSize: e.CodeSize, Refcount: e.Refcount})
	})
}

// ExportSlots streams every slot-count entry through fn. Memory: O(1).
func (t *Tracker) ExportSlots(fn func(SlotSeed) bool) {
	for i := 0; i < ShardCount; i++ {
		sh := t.shards[i]
		sh.mu.RLock()
		for h, c := range sh.slots {
			if !fn(SlotSeed{HashedAddress: h, SlotCount: c}) {
				sh.mu.RUnlock()
				return
			}
		}
		sh.mu.RUnlock()
	}
}

// Close releases resources owned by the tracker (notably the CodeStore's disk
// handles if it is backed by RocksDB). Safe to call multiple times.
func (t *Tracker) Close() error {
	if t.codes != nil {
		err := t.codes.Close()
		t.codes = nil
		return err
	}
	return nil
}
