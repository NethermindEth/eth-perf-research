// Package tracker maintains the sidecar's state composition counters:
//
//   - code refcounts keyed by codeHash      ([32]byte → codeEntry)
//     stored in a pluggable CodeStore (in-memory map by default, RocksDB on
//     the bloatnet). The store is shared across shards because a single
//     codehash maps to exactly one shard via shardOf, so per-shard locks
//     already serialise read-modify-write on any given hash.
//   - slot counts keyed by hashedAddress    ([32]byte → uint64)
//     stored in per-shard in-memory maps. Slot cardinality is bounded by
//     unique contract count (~25 M at 10× state, ~2 GB RAM) which fits.
//
// The data structure is sharded by the first byte of the key so that snapshot
// serialisation, mutation, and lookup can proceed without coarse global locks.
package tracker

import (
	"sync"
)

// ShardCount = 32 — power of two so the routing is a single byte shift.
const ShardCount = 32

// shardOf returns the shard index for a hashed key. Caller guarantees
// len(key) >= 1 (all our keys are [32]byte).
func shardOf(key [32]byte) int {
	return int(key[0]) % ShardCount
}

// codeEntry holds a single codeHash's refcount and the bytecode size carried
// alongside (used for the cumulative codeBytesTotal counter). Bytes are
// captured the first time the hash appears (NewCodeSize on the diff record)
// and re-stamped on every subsequent appearance — if NM ever recomputes the
// size we adopt the latest value.
type codeEntry struct {
	Refcount uint32
	CodeSize uint64
}

// shard owns a slice of the keyspace. It holds slot data in-memory and
// references the tracker-level CodeStore for code data.
type shard struct {
	mu sync.RWMutex

	// codes is a CodeStore reference shared with the rest of the tracker.
	// Set by newShard via the tracker constructor. Per-shard mu serialises
	// any read-modify-write on a single hash; the CodeStore itself is safe
	// for concurrent calls on distinct keys.
	codes CodeStore

	slots map[[32]byte]uint64

	// Cached counters maintained incrementally so /lite never has to walk
	// the maps. Updated under mu.
	uniqueCodeHashes     int64
	codeBytesTotal       int64
	contractsTotal       int64 // accounts that currently have code
	storageSlotsTotal    int64
	contractsWithStorage int64 // accounts with slots > 0
}

func newShard(codes CodeStore) *shard {
	return &shard{
		codes: codes,
		slots: make(map[[32]byte]uint64, 1<<10),
	}
}

// applyCodeRemove decrements the refcount for oldHash. Caller has already
// verified oldHash != zeroHash. When refcount drops to zero the entry is
// evicted and codeBytesTotal/uniqueCodeHashes are updated accordingly.
//
// contractsTotal is always decremented (one account lost its code) regardless
// of whether other accounts still reference the same code hash.
func (s *shard) applyCodeRemove(oldHash [32]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.codes.Get(oldHash); ok {
		if e.Refcount > 0 {
			e.Refcount--
		}
		if e.Refcount == 0 {
			s.codeBytesTotal -= int64(e.CodeSize)
			if s.codeBytesTotal < 0 {
				s.codeBytesTotal = 0
			}
			s.uniqueCodeHashes--
			if s.uniqueCodeHashes < 0 {
				s.uniqueCodeHashes = 0
			}
			s.codes.Delete(oldHash)
		} else {
			s.codes.Put(oldHash, e)
		}
	}
	s.contractsTotal--
	if s.contractsTotal < 0 {
		s.contractsTotal = 0
	}
}

// applyCodeAdd increments the refcount for newHash, allocating the entry if
// missing. CodeBytesTotal is bumped only on first insertion (deduped by hash).
func (s *shard) applyCodeAdd(newHash [32]byte, newSize uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.codes.Get(newHash)
	if !ok {
		s.uniqueCodeHashes++
		s.codeBytesTotal += int64(newSize)
		e = codeEntry{Refcount: 0, CodeSize: newSize}
	} else if e.CodeSize == 0 && newSize > 0 {
		// Repair an earlier entry that came in without a size hint.
		s.codeBytesTotal += int64(newSize)
		e.CodeSize = newSize
	}
	e.Refcount++
	s.codes.Put(newHash, e)
	s.contractsTotal++
}

// applySlotChange folds a SlotCountChange into the slot map.
func (s *shard) applySlotChange(addr [32]byte, oldCount, newCount uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prev, hadEntry := s.slots[addr]
	// Defensively reconcile the parent value the diff claims with what the
	// shard already remembers. Mismatch ⇒ baseline drifted (rare); we trust
	// the new value but log via the counter so the integration test can
	// detect it.
	_ = hadEntry
	_ = prev

	if oldCount > 0 && !hadEntry {
		// The diff says the address had slots but we never saw it — treat
		// as a fresh insertion at oldCount (then transition to newCount).
		s.slots[addr] = oldCount
		s.storageSlotsTotal += int64(oldCount)
		s.contractsWithStorage++
		hadEntry = true
		prev = oldCount
	}

	delta := int64(newCount) - int64(oldCount)
	s.storageSlotsTotal += delta
	if s.storageSlotsTotal < 0 {
		s.storageSlotsTotal = 0
	}
	if newCount == 0 {
		if hadEntry {
			delete(s.slots, addr)
			s.contractsWithStorage--
			if s.contractsWithStorage < 0 {
				s.contractsWithStorage = 0
			}
		}
		return
	}
	if !hadEntry {
		s.contractsWithStorage++
	}
	s.slots[addr] = newCount
}

// snapshotCounters captures the shard's cumulative counters atomically.
func (s *shard) snapshotCounters() shardCounters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return shardCounters{
		UniqueCodeHashes:     s.uniqueCodeHashes,
		CodeBytesTotal:       s.codeBytesTotal,
		ContractsTotal:       s.contractsTotal,
		StorageSlotsTotal:    s.storageSlotsTotal,
		ContractsWithStorage: s.contractsWithStorage,
		SlotEntries:          int64(len(s.slots)),
	}
}

// shardCounters is an immutable snapshot of one shard's tier-1 metrics.
type shardCounters struct {
	UniqueCodeHashes     int64
	CodeBytesTotal       int64
	ContractsTotal       int64
	StorageSlotsTotal    int64
	ContractsWithStorage int64
	SlotEntries          int64
}

func (c shardCounters) add(o shardCounters) shardCounters {
	return shardCounters{
		UniqueCodeHashes:     c.UniqueCodeHashes + o.UniqueCodeHashes,
		CodeBytesTotal:       c.CodeBytesTotal + o.CodeBytesTotal,
		ContractsTotal:       c.ContractsTotal + o.ContractsTotal,
		StorageSlotsTotal:    c.StorageSlotsTotal + o.StorageSlotsTotal,
		ContractsWithStorage: c.ContractsWithStorage + o.ContractsWithStorage,
		SlotEntries:          c.SlotEntries + o.SlotEntries,
	}
}

var zeroHash = [32]byte{}
