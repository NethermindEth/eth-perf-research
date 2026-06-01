package tracker

import (
	"sync"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
)

// applyCodeChangesBatched collapses 2N per-hash cgo crossings into 2 per block
// (one MultiGet + one BatchPut), regardless of CodeHashChange count.
// Each (oldHash, newHash) pair is processed in record order; the working state
// map forwards intermediate values so duplicate-hash collisions within one block
// stay consistent. Zero-refcount hashes become Deletes to keep the store sparse.
// Per-block scratch goes through a sync.Pool to drive allocation near zero
// in the steady state.
func (t *Tracker) applyCodeChangesBatched(changes []rlp.CodeHashChange) {
	if len(changes) == 0 {
		return
	}

	scratch := acquireApplyScratch()
	defer releaseApplyScratch(scratch)

	// Collect unique hashes into state. Using a single map (vs. separate `seen`
	// + `state`) halves the map-churn; a key in `state` already means "seen".
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

	// One cgo crossing: read all existing entries, then fold into the working set.
	entries, present := t.codes.MultiGet(hashList)
	for i, h := range hashList {
		state[h] = applyEntryState{entry: entries[i], exists: present[i]}
	}

	// Replay changes. Shard locks cover counter updates; map-of-value means
	// read-local-mutate-writeback to keep the store sparse.
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
				st.exists = false // becomes a Delete on flush
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
				// Repair entry that arrived without a size hint.
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

	// One BatchPut for live entries; per-call Deletes for zeroed entries (rare).
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

	// Return grown slices to the pool so capacity amortises across blocks.
	scratch.hashList = hashList[:0]
	scratch.putHashes = putHashes[:0]
	scratch.putEntries = putEntries[:0]
}

// applyEntryState is the per-codehash working state for one batched apply.
// Value type (not pointer) so the map allocates one slot per hash.
type applyEntryState struct {
	entry  codeEntry
	exists bool // currently has a non-zero refcount on disk-or-pending
	dirty  bool // mutated in this batch
}

// applyScratch holds per-block scratch reused across calls. Map is cleared
// on release so underlying bucket memory stays allocated.
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
	applyScratchPool.Put(s)
}
