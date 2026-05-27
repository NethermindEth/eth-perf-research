//go:build !nogrocksdb

package tracker

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync/atomic"

	"github.com/linxGnu/grocksdb"
)

// RocksCodeStore is a disk-backed CodeStore that stores one (codehash → 12-byte
// value) record per unique codehash in a RocksDB instance. The 12-byte value
// is layed out as [8-byte big-endian codeSize][4-byte big-endian refcount].
//
// Memory footprint of the live store is bounded by RocksDB's block cache +
// write buffers (default ~256 MB total in OpenRocksCodeStore), independent of
// the number of entries. Reads on a warm cache are ~5 µs; on a cold key
// ~50 µs. At tail-mode diff rates (~100s/sec of CodeHashChange records),
// even the cold-cache path costs <1 % of one CPU.
//
// The store is safe under arbitrary concurrent access (RocksDB handles
// internal synchronisation); the tracker still serialises per-shard so that
// read-modify-write sequences on a single hash are atomic.
type RocksCodeStore struct {
	db    *grocksdb.DB
	opts  *grocksdb.Options
	cache *grocksdb.Cache
	bbto  *grocksdb.BlockBasedTableOptions
	wo    *grocksdb.WriteOptions
	ro    *grocksdb.ReadOptions
	path  string
	count atomic.Int64
}

// OpenRocksCodeStore opens (or creates) a RocksDB-backed CodeStore at dir.
// The directory is created with 0o755 if missing. The store survives process
// restarts; the orchestrator points subsequent boots at the same dir to skip
// rescans.
func OpenRocksCodeStore(dir string) (*RocksCodeStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("OpenRocksCodeStore: empty dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir codestore dir %s: %w", dir, err)
	}

	cache := grocksdb.NewLRUCache(128 << 20) // 128 MB block cache
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(cache)
	bbto.SetCacheIndexAndFilterBlocks(true)
	bbto.SetFilterPolicy(grocksdb.NewBloomFilter(10))

	opts := grocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(true)
	opts.SetWriteBufferSize(64 << 20) // 64 MB write buffer
	opts.SetMaxWriteBufferNumber(2)
	opts.SetTargetFileSizeBase(64 << 20)
	opts.SetMaxOpenFiles(256)
	opts.SetCompression(grocksdb.LZ4Compression)

	db, err := grocksdb.OpenDb(opts, dir)
	if err != nil {
		opts.Destroy()
		bbto.Destroy()
		cache.Destroy()
		return nil, fmt.Errorf("open codestore db %s: %w", dir, err)
	}

	wo := grocksdb.NewDefaultWriteOptions()
	wo.DisableWAL(false) // tail-mode default: durability over speed
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(true)

	s := &RocksCodeStore{
		db:    db,
		opts:  opts,
		cache: cache,
		bbto:  bbto,
		wo:    wo,
		ro:    ro,
		path:  dir,
	}
	// Best-effort initial count via the live-files property. Iterating every
	// row at open time defeats the point of being on-disk; the count is a
	// debug aid only.
	if props := db.GetProperty("rocksdb.estimate-num-keys"); props != "" {
		var n int64
		_, _ = fmt.Sscanf(props, "%d", &n)
		s.count.Store(n)
	}
	return s, nil
}

// encodeEntry packs (codeSize, refcount) into the 12-byte value layout.
func encodeCodeEntry(e codeEntry) [12]byte {
	var buf [12]byte
	binary.BigEndian.PutUint64(buf[0:8], e.CodeSize)
	binary.BigEndian.PutUint32(buf[8:12], e.Refcount)
	return buf
}

func decodeCodeEntry(b []byte) (codeEntry, bool) {
	if len(b) != 12 {
		return codeEntry{}, false
	}
	return codeEntry{
		CodeSize: binary.BigEndian.Uint64(b[0:8]),
		Refcount: binary.BigEndian.Uint32(b[8:12]),
	}, true
}

// Get returns the stored entry for hash, or (zero, false) if absent.
func (s *RocksCodeStore) Get(hash [32]byte) (codeEntry, bool) {
	slice, err := s.db.Get(s.ro, hash[:])
	if err != nil {
		return codeEntry{}, false
	}
	defer slice.Free()
	if !slice.Exists() {
		return codeEntry{}, false
	}
	return decodeCodeEntry(slice.Data())
}

// Put stores (or overwrites) the entry for hash. On any RocksDB error
// (disk-full, hardware) the process exits with log.Fatal — silent
// divergence between the cold tier and in-RAM counters would corrupt
// every downstream metric. Failing fast lets the orchestrator/operator
// react before bad numbers propagate.
func (s *RocksCodeStore) Put(hash [32]byte, e codeEntry) {
	v := encodeCodeEntry(e)
	if err := s.db.Put(s.wo, hash[:], v[:]); err != nil {
		log.Fatalf("codestore put: %v", err)
	}
}

// MultiGet reads N hashes in one cgo call. The keys are passed as a single
// [][]byte to grocksdb's MultiGet, which internally parallelises the
// per-file lookups. At our bloating cadence (thousands of code-hash changes
// per block) this collapses ~2N cgo crossings into 1 and was the bottleneck
// identified by the 2026-05-27 pprof (75% of sidecar CPU in cgocall).
func (s *RocksCodeStore) MultiGet(hashes [][32]byte) (results []codeEntry, present []bool) {
	results = make([]codeEntry, len(hashes))
	present = make([]bool, len(hashes))
	if len(hashes) == 0 {
		return results, present
	}
	keys := make([][]byte, len(hashes))
	for i := range hashes {
		keys[i] = hashes[i][:]
	}
	slices, err := s.db.MultiGet(s.ro, keys...)
	if err != nil {
		// MultiGet returns at most one error covering the whole batch.
		// Behave as if every entry were absent — the caller will fall back
		// to fresh-entry semantics, which is safe.
		return results, present
	}
	for i, sl := range slices {
		if sl.Exists() {
			if e, ok := decodeCodeEntry(sl.Data()); ok {
				results[i] = e
				present[i] = true
			}
		}
		sl.Free()
	}
	return results, present
}

// BatchPut writes all (hash, entry) pairs through one RocksDB WriteBatch.
// Pairs with MultiGet to bound the per-block cgo cost to two crossings
// regardless of how many code-hash changes a block carries.
func (s *RocksCodeStore) BatchPut(hashes [][32]byte, entries []codeEntry) {
	if len(hashes) == 0 {
		return
	}
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()
	for i, h := range hashes {
		v := encodeCodeEntry(entries[i])
		wb.Put(h[:], v[:])
	}
	if err := s.db.Write(s.wo, wb); err != nil {
		log.Fatalf("codestore batch put: %v", err)
	}
}

// Delete removes the entry for hash (no-op if absent). See Put for the
// fail-fast rationale.
func (s *RocksCodeStore) Delete(hash [32]byte) {
	if err := s.db.Delete(s.wo, hash[:]); err != nil {
		log.Fatalf("codestore delete: %v", err)
	}
}

// Iterate calls fn for every entry. Returns the first iterator error, if any.
func (s *RocksCodeStore) Iterate(fn func(hash [32]byte, e codeEntry) bool) error {
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	ro.SetReadaheadSize(2 * 1024 * 1024)
	defer ro.Destroy()
	it := s.db.NewIterator(ro)
	defer it.Close()
	for it.SeekToFirst(); it.Valid(); it.Next() {
		k := it.Key()
		v := it.Value()
		if k.Size() == 32 {
			var h [32]byte
			copy(h[:], k.Data())
			if e, ok := decodeCodeEntry(v.Data()); ok {
				if !fn(h, e) {
					k.Free()
					v.Free()
					break
				}
			}
		}
		k.Free()
		v.Free()
	}
	return it.Err()
}

// Len returns the approximate entry count.
func (s *RocksCodeStore) Len() int64 { return s.count.Load() }

// addCount is called by the streaming bootstrap ingest to maintain Len()
// without an iterator pass.
func (s *RocksCodeStore) addCount(delta int64) { s.count.Add(delta) }

// SetBulkMode toggles WAL durability on Put/Delete. Bulk mode (WAL disabled)
// is appropriate during bootstrap merge-join where we write ~1.4 B records
// in a single pass and a crash means restarting the entire scan anyway. Tail
// mode must keep WAL enabled so a single missed code-hash change is not
// silently lost across a SIGKILL.
//
// Callers must flush via FlushAfterBulk before switching back to durable
// mode so any in-memory writes settle to SST.
func (s *RocksCodeStore) SetBulkMode(bulk bool) {
	if s == nil || s.wo == nil {
		return
	}
	s.wo.DisableWAL(bulk)
}

// FlushAfterBulk forces any pending writes to flush to SST. Pair with
// SetBulkMode(false) at the end of bootstrap before tail begins.
func (s *RocksCodeStore) FlushAfterBulk() error {
	if s == nil || s.db == nil {
		return nil
	}
	fo := grocksdb.NewDefaultFlushOptions()
	fo.SetWait(true)
	defer fo.Destroy()
	return s.db.Flush(fo)
}

// Close releases the underlying DB handle and supporting structures.
func (s *RocksCodeStore) Close() error {
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
	if s.wo != nil {
		s.wo.Destroy()
		s.wo = nil
	}
	if s.ro != nil {
		s.ro.Destroy()
		s.ro = nil
	}
	if s.opts != nil {
		s.opts.Destroy()
		s.opts = nil
	}
	if s.bbto != nil {
		s.bbto.Destroy()
		s.bbto = nil
	}
	if s.cache != nil {
		s.cache.Destroy()
		s.cache = nil
	}
	return nil
}

// BatchIngest provides an efficient bulk-load path used by the bootstrap
// merge-join. Caller streams entries via Add; the batch flushes to RocksDB
// every batchSize records and finally on Close.
type BatchIngest struct {
	store     *RocksCodeStore
	wb        *grocksdb.WriteBatch
	batchSize int
	pending   int
	added     int64
}

// NewBatchIngest opens a batched writer. batchSize == 0 picks a sensible
// default (8192 records ≈ 384 KB per batch given the 44 B record).
func (s *RocksCodeStore) NewBatchIngest(batchSize int) *BatchIngest {
	if batchSize <= 0 {
		batchSize = 8192
	}
	return &BatchIngest{
		store:     s,
		wb:        grocksdb.NewWriteBatch(),
		batchSize: batchSize,
	}
}

// Add queues one (hash, entry) Put. Returns an error if a flush fails.
func (b *BatchIngest) Add(hash [32]byte, e codeEntry) error {
	v := encodeCodeEntry(e)
	b.wb.Put(hash[:], v[:])
	b.pending++
	b.added++
	if b.pending >= b.batchSize {
		return b.flush()
	}
	return nil
}

func (b *BatchIngest) flush() error {
	if b.pending == 0 {
		return nil
	}
	if err := b.store.db.Write(b.store.wo, b.wb); err != nil {
		return fmt.Errorf("codestore batch write: %w", err)
	}
	b.wb.Clear()
	b.pending = 0
	return nil
}

// Close flushes any remaining batch and releases resources. Updates the
// store's Len() with the total added.
func (b *BatchIngest) Close() error {
	defer b.wb.Destroy()
	if err := b.flush(); err != nil {
		return err
	}
	b.store.addCount(b.added)
	return nil
}

// Added returns the number of entries enqueued via Add (regardless of flush
// state). Useful for progress reporting from the caller.
func (b *BatchIngest) Added() int64 { return b.added }
