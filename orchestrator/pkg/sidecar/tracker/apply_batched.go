package tracker

import (
	"sync"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
)

// applyCodeChangesBatched is the batched fast path replacing the per-change
// shard.applyCodeAdd / shard.applyCodeRemove fan-out. Pprof on 2026-05-27
// showed 75% of sidecar CPU sitting in runtime.cgocall — almost entirely
// per-hash codestore.Get and Put. This collapses 2N cgo crossings into 2
// per block (one MultiGet + one BatchPut), independent of how many
// CodeHashChange entries a block carries.
//
// Semantics are unchanged vs. the single-change path:
//   - Each (oldHash, newHash) pair is processed in record order.
//   - Refcount transitions and shard counters stay consistent across
//     duplicate-hash collisions inside one block (the working state map
//     forwards intermediate values).
//   - Hashes whose refcount drops to zero are issued as Deletes — not Puts
//     — to keep the on-disk store sparse.
//
// Allocation strategy: every per-block scratch (state map, hash list, put
// arrays) goes through a sync.Pool. Pprof alloc_space 2026-05-27 had 46%
// of sidecar allocations on this hot path's per-call maps; pooling drives
// that to ~0 for the steady-state case where consecutive blocks have
// similar code-change counts. Caller drains the pooled scratches at end.
func (t *Tracker) applyCodeChangesBatched(changes []rlp.CodeHashChange) {
	if len(changes) == 0 {
		return
	}

	scratch := acquireApplyScratch()
	defer releaseApplyScratch(scratch)

	// 1. Deduplicate hashes via the same map that will hold their state.
	// The `seen` set from the prior implementation was redundant — once a
	// hash key is in `state`, that already records "seen". Folding the two
	// maps into one drops half the map churn.
	state := scratch.state
	hashList := scratch.hashList
	for _, c := range changes {
		if c.OldHash != zeroHash {
			if _, ok := state[c.OldHash]; !ok {
				e, p := codeEntry{}, false
				state[c.OldHash] = applyEntryState{entry: e, exists: p}
				hashList = append(hashList, c.OldHash)
			}
		}
		if c.NewHash != zeroHash {
			if _, ok := state[c.NewHash]; !ok {
				e, p := codeEntry{}, false
				state[c.NewHash] = applyEntryState{entry: e, exists: p}
				hashList = append(hashList, c.NewHash)
			}
		}
	}

	// 2. One cgo crossing into RocksDB to materialise the existing state,
	// then fold the result back into the in-flight working set.
	entries, present := t.codes.MultiGet(hashList)
	for i, h := range hashList {
		state[h] = applyEntryState{entry: entries[i], exists: present[i]}
	}

	// 3. Replay changes against the in-memory state. Shard locks still cover
	// the counter updates so other goroutines reading shard counters see a
	// consistent point-in-time view, just as before. Map-of-value (not
	// map-of-pointer) means we read, mutate the local copy, then write back.
	for _, c := range changes {
		if c.OldHash != zeroHash {
			st := state[c.OldHash]
			s := t.shards[shardOf(c.OldHash)]
			s.mu.Lock()
			if st.exists && st.entry.Refcount > 0 {
				st.entry.Refcount--
			}
			if st.exists && st.entry.Refcount == 0 {
				s.codeBytesTotal -= int64(st.entry.CodeSize)
				if s.codeBytesTotal < 0 {
					s.codeBytesTotal = 0
				}
				s.uniqueCodeHashes--
				if s.uniqueCodeHashes < 0 {
					s.uniqueCodeHashes = 0
				}
				st.exists = false // will become a Delete on flush
			}
			s.contractsTotal--
			if s.contractsTotal < 0 {
				s.contractsTotal = 0
			}
			st.dirty = true
			s.mu.Unlock()
			state[c.OldHash] = st
		}
		if c.NewHash != zeroHash {
			st := state[c.NewHash]
			s := t.shards[shardOf(c.NewHash)]
			s.mu.Lock()
			if !st.exists {
				s.uniqueCodeHashes++
				s.codeBytesTotal += int64(c.NewCodeSize)
				st.entry = codeEntry{Refcount: 0, CodeSize: c.NewCodeSize}
			} else if st.entry.CodeSize == 0 && c.NewCodeSize > 0 {
				// Repair an earlier entry that came in without a size hint.
				s.codeBytesTotal += int64(c.NewCodeSize)
				st.entry.CodeSize = c.NewCodeSize
			}
			st.entry.Refcount++
			st.exists = true
			s.contractsTotal++
			st.dirty = true
			s.mu.Unlock()
			state[c.NewHash] = st
		}
	}

	// 4. Flush dirty state. Live entries go through one BatchPut (one cgo
	// crossing); refcount-zero entries go through Delete (per-call, but
	// these are rare relative to inserts/updates).
	putHashes := scratch.putHashes
	putEntries := scratch.putEntries
	for _, h := range hashList {
		st := state[h]
		if !st.dirty {
			continue
		}
		if st.exists {
			putHashes = append(putHashes, h)
			putEntries = append(putEntries, st.entry)
		} else {
			t.codes.Delete(h)
		}
	}
	if len(putHashes) > 0 {
		t.codes.BatchPut(putHashes, putEntries)
	}

	// Stash the (possibly grown) slices back so the pool gets the larger
	// capacity for the next block — amortises the growth cost across
	// future allocations.
	scratch.hashList = hashList[:0]
	scratch.putHashes = putHashes[:0]
	scratch.putEntries = putEntries[:0]
}

// applyEntryState is the per-codehash working state during one batched apply.
// Value type so the state map allocates one entry per hash, not one entry
// plus one heap-pointer-target per hash.
type applyEntryState struct {
	entry  codeEntry
	exists bool // currently has a non-zero refcount on disk-or-pending
	dirty  bool // mutated in this batch
}

// applyScratch holds the scratch slices/maps reused across consecutive
// applyCodeChangesBatched calls. The map gets cleared on release so the
// underlying buckets stay allocated.
type applyScratch struct {
	state      map[[32]byte]applyEntryState
	hashList   [][32]byte
	putHashes  [][32]byte
	putEntries []codeEntry
}

var applyScratchPool = sync.Pool{
	New: func() any {
		return &applyScratch{
			state:      make(map[[32]byte]applyEntryState, 256),
			hashList:   make([][32]byte, 0, 256),
			putHashes:  make([][32]byte, 0, 256),
			putEntries: make([]codeEntry, 0, 256),
		}
	},
}

func acquireApplyScratch() *applyScratch {
	return applyScratchPool.Get().(*applyScratch)
}

func releaseApplyScratch(s *applyScratch) {
	clear(s.state)
	// hashList/putHashes/putEntries already truncated by caller before defer.
	applyScratchPool.Put(s)
}
