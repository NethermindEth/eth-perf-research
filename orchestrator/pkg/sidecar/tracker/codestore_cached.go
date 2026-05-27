package tracker

import "sync"

// CachedCodeStore wraps a slower (typically disk-backed) CodeStore with an
// in-memory map cache that records both presence and value. The sidecar is
// the only writer to the underlying store, so the cache stays coherent
// without any cross-process invalidation.
//
// Why this exists: after Option A (per-block MultiGet+BatchPut) bounded the
// cgo crossings to 2 per block, the residual cost is still RocksDB lookups
// inside MultiGet — block-cache hits at ~1 µs, misses at tens of µs. With
// fresh-deploy bloating workloads, the same hash rarely repeats, so the
// cache mainly helps by caching the "absent" answer so the second sighting
// of a hash skips RocksDB entirely.
//
// Memory: unbounded by entry count. At the observed bloating cadence the
// cache grows ~520 k entries / day at ~80 B each ≈ 40 MB / day. The
// snapshot loop periodically calls Iterate on the underlying store, not on
// the cache, so the cache never becomes the source of truth.
type CachedCodeStore struct {
	inner CodeStore
	mu    sync.RWMutex
	cache map[[32]byte]cachedEntry
}

type cachedEntry struct {
	entry   codeEntry
	present bool
}

// NewCachedCodeStore returns a write-through cache around inner.
func NewCachedCodeStore(inner CodeStore) *CachedCodeStore {
	return &CachedCodeStore{
		inner: inner,
		cache: make(map[[32]byte]cachedEntry, 1<<14),
	}
}

func (s *CachedCodeStore) Get(hash [32]byte) (codeEntry, bool) {
	s.mu.RLock()
	v, ok := s.cache[hash]
	s.mu.RUnlock()
	if ok {
		return v.entry, v.present
	}
	e, p := s.inner.Get(hash)
	s.mu.Lock()
	s.cache[hash] = cachedEntry{entry: e, present: p}
	s.mu.Unlock()
	return e, p
}

func (s *CachedCodeStore) MultiGet(hashes [][32]byte) (results []codeEntry, present []bool) {
	results = make([]codeEntry, len(hashes))
	present = make([]bool, len(hashes))
	if len(hashes) == 0 {
		return results, present
	}

	// First pass: serve everything we can from cache, build the miss list.
	var missIdx []int
	var missHashes [][32]byte
	s.mu.RLock()
	for i, h := range hashes {
		if v, ok := s.cache[h]; ok {
			results[i] = v.entry
			present[i] = v.present
		} else {
			missIdx = append(missIdx, i)
			missHashes = append(missHashes, h)
		}
	}
	s.mu.RUnlock()

	if len(missHashes) == 0 {
		return results, present
	}

	// Second pass: one MultiGet to the inner store for misses, then
	// populate the cache so the next sighting of any of these hashes
	// stays in Go memory.
	innerEntries, innerPresent := s.inner.MultiGet(missHashes)
	s.mu.Lock()
	for j, i := range missIdx {
		results[i] = innerEntries[j]
		present[i] = innerPresent[j]
		s.cache[missHashes[j]] = cachedEntry{entry: innerEntries[j], present: innerPresent[j]}
	}
	s.mu.Unlock()
	return results, present
}

func (s *CachedCodeStore) Put(hash [32]byte, e codeEntry) {
	s.mu.Lock()
	s.cache[hash] = cachedEntry{entry: e, present: true}
	s.mu.Unlock()
	s.inner.Put(hash, e)
}

func (s *CachedCodeStore) BatchPut(hashes [][32]byte, entries []codeEntry) {
	if len(hashes) == 0 {
		return
	}
	s.mu.Lock()
	for i, h := range hashes {
		s.cache[h] = cachedEntry{entry: entries[i], present: true}
	}
	s.mu.Unlock()
	s.inner.BatchPut(hashes, entries)
}

func (s *CachedCodeStore) Delete(hash [32]byte) {
	s.mu.Lock()
	s.cache[hash] = cachedEntry{present: false}
	s.mu.Unlock()
	s.inner.Delete(hash)
}

// Iterate passes through to the underlying store. The cache is a strict
// subset of (or equal to) the inner store for present-keyed entries, so
// callers that need a full walk (snapshot writer, restart rehydration)
// must hit the source of truth, not the partial cache.
func (s *CachedCodeStore) Iterate(fn func(hash [32]byte, e codeEntry) bool) error {
	return s.inner.Iterate(fn)
}

func (s *CachedCodeStore) Len() int64 { return s.inner.Len() }

func (s *CachedCodeStore) Close() error { return s.inner.Close() }

// CacheStats returns the live cache size; used by the metrics endpoint and
// debug logging when the cache is suspected of unbounded growth.
func (s *CachedCodeStore) CacheStats() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cache)
}
