package tracker

import "sync"

// CachedCodeStore wraps a disk-backed CodeStore with an in-memory map.
// The sidecar is the sole writer, so no cross-process invalidation is needed.
//
// Rationale: MultiGet costs ~1 µs (cache hit) to tens of µs (miss). At
// bloating cadence the same hash rarely repeats, so the main win is caching
// "absent" answers to skip RocksDB on second sightings.
//
// Memory grows ~40 MB/day at observed bloating rates. Iterate delegates to
// the underlying store, which is always the source of truth.
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

	// One MultiGet for misses, then cache results.
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

// Iterate delegates to the underlying store — the cache is a subset,
// not the source of truth.
func (s *CachedCodeStore) Iterate(fn func(hash [32]byte, e codeEntry) bool) error {
	return s.inner.Iterate(fn)
}

func (s *CachedCodeStore) Len() int64 { return s.inner.Len() }

func (s *CachedCodeStore) Close() error { return s.inner.Close() }

// CacheStats returns the live cache size for debug logging.
func (s *CachedCodeStore) CacheStats() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.cache)
}
