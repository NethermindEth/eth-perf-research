package tracker

import (
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
func (t *Tracker) applyCodeChangesBatched(changes []rlp.CodeHashChange) {
	if len(changes) == 0 {
		return
	}

	// 1. Deduplicate hashes we need to look up. The same codehash may appear
	// in both an oldHash and a newHash slot across different changes; one
	// MultiGet round-trip suffices.
	hashList := make([][32]byte, 0, 2*len(changes))
	seen := make(map[[32]byte]struct{}, 2*len(changes))
	for _, c := range changes {
		if c.OldHash != zeroHash {
			if _, ok := seen[c.OldHash]; !ok {
				seen[c.OldHash] = struct{}{}
				hashList = append(hashList, c.OldHash)
			}
		}
		if c.NewHash != zeroHash {
			if _, ok := seen[c.NewHash]; !ok {
				seen[c.NewHash] = struct{}{}
				hashList = append(hashList, c.NewHash)
			}
		}
	}

	// 2. One cgo crossing into RocksDB to materialise the existing state.
	entries, present := t.codes.MultiGet(hashList)

	// 3. Build the working state. exists reflects the LIVE refcount > 0
	// invariant: false means either never-seen or last write was a Delete.
	type entryState struct {
		entry  codeEntry
		exists bool // currently has a non-zero refcount on disk-or-pending
		dirty  bool // mutated in this batch
	}
	state := make(map[[32]byte]*entryState, len(hashList))
	for i, h := range hashList {
		state[h] = &entryState{entry: entries[i], exists: present[i]}
	}

	// 4. Replay changes against the in-memory state. Shard locks still cover
	// the counter updates so other goroutines reading shard counters see a
	// consistent point-in-time view, just as before.
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
		}
	}

	// 5. Flush dirty state. Live entries go through one BatchPut (one cgo
	// crossing); refcount-zero entries go through Delete (per-call, but
	// these are rare relative to inserts/updates).
	putHashes := make([][32]byte, 0, len(state))
	putEntries := make([]codeEntry, 0, len(state))
	for h, st := range state {
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
}
