package tracker

import "sync"

// CodeStore is the per-codehash backing store the tracker uses to remember
// (refcount, codeSize) for every codehash ever observed. Two implementations
// exist:
//
//   - memCodeStore: a plain in-RAM map. Used by unit tests and small
//     deployments where the cardinality is bounded.
//   - rocksCodeStore: an LSM-backed store (see codestore_rocks.go) used at
//     bloatnet scale (1.4 B+ codehashes), where the in-RAM footprint of all
//     entries would exceed available memory.
//
// The store is keyed by the canonical 32-byte codehash. Implementations must
// be safe under concurrent access from goroutines holding *different* shard
// locks; the tracker's per-shard mu already serialises operations on any
// given codehash, so the store only needs to handle racing operations on
// distinct hashes.
type CodeStore interface {
	// Get returns the stored entry for hash, or (zero, false) if absent.
	Get(hash [32]byte) (codeEntry, bool)
	// MultiGet reads N hashes in one shot. results[i] is the entry for
	// hashes[i] (zero value if absent); present[i] reports whether the entry
	// existed. For disk-backed stores this collapses N cgo crossings into
	// one and lets the implementation parallelise across L0 files.
	MultiGet(hashes [][32]byte) (results []codeEntry, present []bool)
	// Put stores (or overwrites) the entry for hash.
	Put(hash [32]byte, e codeEntry)
	// BatchPut writes all (hash, entry) pairs as one atomic operation.
	// Same single-cgo-crossing motivation as MultiGet.
	BatchPut(hashes [][32]byte, entries []codeEntry)
	// Delete removes the entry for hash (no-op if absent).
	Delete(hash [32]byte)
	// Iterate calls fn for every entry. Returning false from fn aborts the
	// walk. The store may iterate in any order. While iterating, the caller
	// must not invoke Put/Delete on the same store; the tracker only ever
	// iterates during snapshot.Write/ExportKeys which run while no diffs are
	// being applied (snapshot loop holds the tracker exclusively).
	Iterate(fn func(hash [32]byte, e codeEntry) bool) error
	// Len returns the current entry count. May be approximate for LSM-backed
	// stores; consumers use it only for logging.
	Len() int64
	// Close releases any resources held by the store (file handles, mmaps,
	// etc.). After Close the store must not be used.
	Close() error
}

// newMemCodeStore is the default in-memory implementation. Used by tests and
// by the production tracker until ReplaceCodeStore swaps in a disk-backed
// store during bootstrap.
func newMemCodeStore() *memCodeStore {
	return &memCodeStore{m: make(map[[32]byte]codeEntry, 1<<10)}
}

type memCodeStore struct {
	mu sync.RWMutex
	m  map[[32]byte]codeEntry
}

func (s *memCodeStore) Get(hash [32]byte) (codeEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[hash]
	return e, ok
}

func (s *memCodeStore) MultiGet(hashes [][32]byte) (results []codeEntry, present []bool) {
	results = make([]codeEntry, len(hashes))
	present = make([]bool, len(hashes))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i, h := range hashes {
		e, ok := s.m[h]
		if ok {
			results[i] = e
			present[i] = true
		}
	}
	return results, present
}

func (s *memCodeStore) Put(hash [32]byte, e codeEntry) {
	s.mu.Lock()
	s.m[hash] = e
	s.mu.Unlock()
}

func (s *memCodeStore) BatchPut(hashes [][32]byte, entries []codeEntry) {
	s.mu.Lock()
	for i, h := range hashes {
		s.m[h] = entries[i]
	}
	s.mu.Unlock()
}

func (s *memCodeStore) Delete(hash [32]byte) {
	s.mu.Lock()
	delete(s.m, hash)
	s.mu.Unlock()
}

func (s *memCodeStore) Iterate(fn func(hash [32]byte, e codeEntry) bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for h, e := range s.m {
		if !fn(h, e) {
			return nil
		}
	}
	return nil
}

func (s *memCodeStore) Len() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.m))
}

func (s *memCodeStore) Close() error { return nil }
