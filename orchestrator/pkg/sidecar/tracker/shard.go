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

// ShardCount is 32 (power of two); routing is a single byte shift.
const ShardCount = 32

func shardOf(key [32]byte) int {
	return int(key[0]) % ShardCount
}

// codeEntry holds a codeHash's refcount and bytecode size (used for codeBytesTotal).
// Size is captured on first insertion and re-stamped on each subsequent sighting.
type codeEntry struct {
	Refcount uint32
	CodeSize uint64
}

// shard owns a slice of the keyspace. Per-shard mu serialises read-modify-write
// on any given hash; the CodeStore is safe under concurrent distinct-key access.
type shard struct {
	mu    sync.RWMutex
	codes CodeStore

	slots map[[32]byte]uint64

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

// applyCodeRemove decrements the refcount for oldHash (caller guarantees
// oldHash != zeroHash). On zero refcount the entry is evicted. contractsTotal
// is always decremented regardless of other accounts sharing the same hash.
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

// applyCodeAdd increments refcount for newHash. CodeBytesTotal only grows on
// first insertion (deduped by hash).
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
	_ = hadEntry
	_ = prev

	if oldCount > 0 && !hadEntry {
		// Address had slots but we never saw it — insert at oldCount before applying delta.
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

func (s *shard) snapshotCounters() shardCounters {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return shardCounters{
		UniqueCodeHashes:     s.uniqueCodeHashes,
		CodeBytesTotal:       s.codeBytesTotal,
		ContractsTotal:       s.contractsTotal,
		StorageSlotsTotal:    s.storageSlotsTotal,
		ContractsWithStorage: s.contractsWithStorage,
	}
}

type shardCounters struct {
	UniqueCodeHashes     int64
	CodeBytesTotal       int64
	ContractsTotal       int64
	StorageSlotsTotal    int64
	ContractsWithStorage int64
}

func (c shardCounters) add(o shardCounters) shardCounters {
	return shardCounters{
		UniqueCodeHashes:     c.UniqueCodeHashes + o.UniqueCodeHashes,
		CodeBytesTotal:       c.CodeBytesTotal + o.CodeBytesTotal,
		ContractsTotal:       c.ContractsTotal + o.ContractsTotal,
		StorageSlotsTotal:    c.StorageSlotsTotal + o.StorageSlotsTotal,
		ContractsWithStorage: c.ContractsWithStorage + o.ContractsWithStorage,
	}
}

var zeroHash = [32]byte{}
