//go:build !nogrocksdb

// Package scanner ports Prototype A's bootstrap scanner into a reusable module.
//
// The scanner walks the FlatDb's StateNodes + StorageNodes column families
// once, classifies every node, decodes account leaves, and emits a fully
// seeded Tracker plus tier-2 scan counters. Memory: O(unique-code-hashes +
// unique-contracts) — backed by on-disk spill files so the process RSS stays
// bounded even on the bloatnet's 1.4 TB FlatDb.
package scanner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linxGnu/grocksdb"
	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/db"
	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// FlatInTrieStorageKeyLen is the canonical FlatDb storage key length:
//
//	[addrHash[0..4]] ++ [pathBytes[0..8]] ++ [addrHash[4..20]]  = 28 bytes.
const FlatInTrieStorageKeyLen = 28

// Options drives the scanner.
type Options struct {
	SpillDir   string // where to drop per-contract slot tally + code-hash spill files
	SkipCode   bool   // skip the Code DB lookup pass (faster smoke tests)
	CodeDBPath string
	BlockCache int
	UseMmap    bool

	// DedupDir is the on-disk location of the temporary RocksDB used for the
	// streaming codehash dedup. Created fresh (any prior contents removed)
	// and deleted on success. Disk usage ≈ 1.5× the codehash spill (LSM
	// overhead) — at 10× state this can reach ~700 GB, so callers should
	// point this at a roomy volume. Empty string ⇒ <SpillDir>/codehash-dedup.
	DedupDir string

	// CodeStoreDir, when non-empty, swaps the tracker's default in-memory
	// CodeStore for a persistent RocksDB-backed one rooted at this path,
	// and routes the bootstrap merge-join through a streaming sink so the
	// per-codehash entries never form a Go slice. Required at bloatnet
	// scale (>~50 M codehashes) to avoid OOM during the tracker-seed step.
	// Empty ⇒ keep the in-memory store (compatible with small deployments
	// and unit tests).
	CodeStoreDir string
}

// Result is the scanner's output. Tier-2 counters are absolute; the seeded
// tracker carries the per-key state.
type Result struct {
	BlockNumber int64
	StateRoot   [32]byte
	Tracker     *tracker.Tracker
	Counters    tracker.ScanCounters

	ElapsedMs    int64
	AccountNodes int64
	StorageNodes int64
}

// Run performs the full bootstrap scan against the open DB handle.
func Run(ctx context.Context, h *db.Handle, opts Options, log zerolog.Logger) (Result, error) {
	if h == nil || h.StateCF == nil || h.StorageCF == nil {
		return Result{}, fmt.Errorf("scanner: missing CF handles")
	}
	start := time.Now()

	spillDir := opts.SpillDir
	if spillDir == "" {
		spillDir = os.TempDir()
	}

	st := newStats()
	tracker0 := tracker.New()

	// Swap the default in-memory CodeStore for the disk-backed one when the
	// operator points us at a roomy volume. This is the load-bearing step
	// for bloatnet bootstraps: 1.4 B codehashes × 12 B (refcount + size)
	// ≈ 16 GB on-disk, but only a few hundred MB resident at any time.
	var rocksStore *tracker.RocksCodeStore
	if opts.CodeStoreDir != "" {
		store, oerr := tracker.OpenRocksCodeStore(opts.CodeStoreDir)
		if oerr != nil {
			return Result{}, fmt.Errorf("open codestore at %s: %w", opts.CodeStoreDir, oerr)
		}
		tracker0.UseCodeStore(store)
		rocksStore = store
		// Disable WAL during the merge-join: a bootstrap crash forces a
		// rescan anyway, so per-record fsync cost is pure waste here. We
		// flip durability back on at the end of seed ingest and flush
		// memtables to SST before tail starts.
		store.SetBulkMode(true)
	}

	addrSpill, err := openSpillFile(spillDir, "slot-spill-")
	if err != nil {
		return Result{}, fmt.Errorf("init slot spill: %w", err)
	}
	defer addrSpill.cleanup()

	codeHashSpill, err := openSpillFile(spillDir, "codehash-spill-")
	if err != nil {
		return Result{}, fmt.Errorf("init code-hash spill: %w", err)
	}
	defer codeHashSpill.cleanup()

	progressDone := make(chan struct{})
	go runProgress(ctx, st, log, progressDone)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanAccountCF(ctx, h.DB, h.StateCF, st, codeHashSpill)
	}()
	go func() {
		defer wg.Done()
		scanStorageCF(ctx, h.DB, h.StorageCF, st, addrSpill)
	}()
	wg.Wait()
	close(progressDone)

	// Aggregate per-contract slot counts and slot-count histogram from the
	// disk spill — bounded memory regardless of contract count.
	hist, contractsWithStorage, slotSeeds, err := aggregateHistogram(addrSpill.path, log)
	if err != nil {
		return Result{}, fmt.Errorf("histogram aggregation: %w", err)
	}

	// Resolve code sizes from the Code DB via streaming dedup. Constant
	// memory regardless of unique-codehash cardinality — the old in-memory
	// loadUniqueSorted path OOMed at bloatnet scale (~1.37 B unique entries).
	if err := codeHashSpill.flush(); err != nil {
		log.Warn().Err(err).Msg("codehash spill flush")
	}
	var codeBytes int64
	var uniqueCodeHashes int64

	// Stream seeds straight into the tracker; never materialise a []CodeSeed.
	codeSink := func(cs tracker.CodeSeed) error {
		tracker0.SeedOneCode(cs)
		return nil
	}
	if !opts.SkipCode && opts.CodeDBPath != "" {
		dedupDir := opts.DedupDir
		if dedupDir == "" {
			dedupDir = filepath.Join(spillDir, "codehash-dedup")
		}
		cb, hitsCount, uniq, missing, lerr := streamingDedupAndLookupSink(
			codeHashSpill.path,
			opts.CodeDBPath,
			dedupDir,
			opts.BlockCache,
			opts.UseMmap,
			log,
			codeSink,
		)
		if lerr != nil {
			log.Warn().Err(lerr).Msg("streaming dedup+lookup")
		}
		codeBytes = cb
		uniqueCodeHashes = uniq
		if missing > 0 {
			log.Warn().
				Int64("missing", missing).
				Msg("codehashes referenced by accounts but absent in Code DB — possible data integrity issue")
		}
		_ = hitsCount
	} else {
		// SkipCode (or no CodeDB): we still need to know the unique cardinality
		// and emit refcount=1 seeds so contractsTotal == accounts with code.
		// Drain the dedup DB without doing the codeDB merge-join.
		dedupDir := opts.DedupDir
		if dedupDir == "" {
			dedupDir = filepath.Join(spillDir, "codehash-dedup")
		}
		uniq, derr := drainDedupSeedsStreaming(codeHashSpill.path, dedupDir, log, codeSink)
		if derr != nil {
			log.Warn().Err(derr).Msg("dedup drain (skip-code path)")
		}
		uniqueCodeHashes = uniq
	}

	// Slot seeds remain in-memory: cardinality is bounded by unique
	// contracts (~25 M at 10× state) and the tracker holds them per-shard,
	// which fits in ~2 GB. If that ceiling ever bites we add a SlotStore
	// analogous to CodeStore.
	for _, ss := range slotSeeds {
		tracker0.SeedOneSlot(ss)
	}

	// Flip the codestore back to durable mode and force-flush so the cold
	// tier is on-disk before the tracker is exposed to the tailer.
	if rocksStore != nil {
		rocksStore.SetBulkMode(false)
		if ferr := rocksStore.FlushAfterBulk(); ferr != nil {
			log.Warn().Err(ferr).Msg("codestore flush after bulk")
		}
	}

	counters := tracker.ScanCounters{
		BlockNumber:           0, // metadata-derived block number TBD
		AccountsTotal:         st.accountsTotal.Load(),
		EmptyAccounts:         st.emptyAccounts.Load(),
		AccountTrieBranches:   st.accountFull.Load(),
		AccountTrieExtensions: st.accountShort.Load(),
		AccountTrieLeaves:     st.accountValue.Load(),
		AccountTrieBytes:      st.accountBytes.Load(),
		StorageTrieBranches:   st.storageFull.Load(),
		StorageTrieExtensions: st.storageShort.Load(),
		StorageTrieLeaves:     st.storageValue.Load(),
		StorageTrieBytes:      st.storageBytes.Load(),
		SlotHistogram:         hist[:],
	}
	_ = contractsWithStorage
	_ = codeBytes // counters carried by per-shard refcount × CodeSize

	// Best-effort: stamp metadata before applying counters so the seeded
	// codeBytesTotal reflects bytes the code-DB returned.
	tracker0.SetScanCounters(counters)
	_ = uniqueCodeHashes

	return Result{
		BlockNumber:  0,
		Tracker:      tracker0,
		Counters:     counters,
		ElapsedMs:    time.Since(start).Milliseconds(),
		AccountNodes: st.accountFull.Load() + st.accountShort.Load() + st.accountValue.Load(),
		StorageNodes: st.storageFull.Load() + st.storageShort.Load() + st.storageValue.Load(),
	}, nil
}

// --- internal helpers ---

type stats struct {
	accountFull      atomic.Int64
	accountShort     atomic.Int64
	accountValue     atomic.Int64
	accountBytes     atomic.Int64
	accountsTotal    atomic.Int64
	contractsTotal   atomic.Int64
	emptyAccounts    atomic.Int64
	accountMalformed atomic.Int64

	storageFull       atomic.Int64
	storageShort      atomic.Int64
	storageValue      atomic.Int64
	storageBytes      atomic.Int64
	storageSlotsTotal atomic.Int64
	storageNoAttrib   atomic.Int64
}

func newStats() *stats { return &stats{} }

func runProgress(ctx context.Context, st *stats, log zerolog.Logger, done <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			elapsed := time.Since(start).Seconds()
			accNodes := st.accountFull.Load() + st.accountShort.Load() + st.accountValue.Load()
			stoNodes := st.storageFull.Load() + st.storageShort.Load() + st.storageValue.Load()
			log.Info().
				Float64("elapsed_s", elapsed).
				Int64("account_nodes", accNodes).
				Int64("storage_nodes", stoNodes).
				Int64("accounts", st.accountsTotal.Load()).
				Int64("contracts", st.contractsTotal.Load()).
				Int64("slots", st.storageSlotsTotal.Load()).
				Msg("scanner progress")
		}
	}
}

func scanAccountCF(ctx context.Context, gdb *grocksdb.DB, cf *grocksdb.ColumnFamilyHandle,
	st *stats, codeHashes *spillFile) {
	ro := db.NewScanReadOptions()
	defer ro.Destroy()
	it := gdb.NewIteratorCF(ro, cf)
	defer it.Close()

	for it.SeekToFirst(); it.Valid(); it.Next() {
		if ctx.Err() != nil {
			return
		}
		v := it.Value()
		val := v.Data()
		st.accountBytes.Add(int64(len(val)))
		kind, leafVal := rlppkg.Classify(val)
		switch kind {
		case rlppkg.NodeBranch:
			st.accountFull.Add(1)
		case rlppkg.NodeExtension:
			st.accountShort.Add(1)
		case rlppkg.NodeLeaf:
			st.accountValue.Add(1)
			st.accountsTotal.Add(1)
			info, ok := rlppkg.DecodeAccount(leafVal)
			if !ok {
				st.accountMalformed.Add(1)
			} else {
				if info.IsEmpty {
					st.emptyAccounts.Add(1)
				}
				if info.HasCode {
					st.contractsTotal.Add(1)
					_ = codeHashes.appendHash(info.CodeHash)
				}
			}
		default:
			st.accountMalformed.Add(1)
		}
		v.Free()
	}
}

func scanStorageCF(ctx context.Context, gdb *grocksdb.DB, cf *grocksdb.ColumnFamilyHandle,
	st *stats, addrs *spillFile) {
	ro := db.NewScanReadOptions()
	defer ro.Destroy()
	it := gdb.NewIteratorCF(ro, cf)
	defer it.Close()

	for it.SeekToFirst(); it.Valid(); it.Next() {
		if ctx.Err() != nil {
			return
		}
		k := it.Key()
		v := it.Value()
		key := k.Data()
		val := v.Data()
		st.storageBytes.Add(int64(len(val)))
		kind, _ := rlppkg.Classify(val)
		switch kind {
		case rlppkg.NodeBranch:
			st.storageFull.Add(1)
		case rlppkg.NodeExtension:
			st.storageShort.Add(1)
		case rlppkg.NodeLeaf:
			st.storageValue.Add(1)
			st.storageSlotsTotal.Add(1)
			if len(key) == FlatInTrieStorageKeyLen {
				// Reconstruct the truncated 20-byte address hash and pad to
				// the [32]byte form the tracker expects.
				var padded [32]byte
				copy(padded[0:4], key[0:4])
				copy(padded[4:20], key[12:28])
				_ = addrs.appendHash(padded)
			} else {
				st.storageNoAttrib.Add(1)
			}
		}
		k.Free()
		v.Free()
	}
}

// --- spill file (32-byte records) ---

type spillFile struct {
	f     *os.File
	buf   []byte
	path  string
	count int64
}

func openSpillFile(dir, prefix string) (*spillFile, error) {
	f, err := os.CreateTemp(dir, prefix+"*.bin")
	if err != nil {
		return nil, err
	}
	return &spillFile{f: f, buf: make([]byte, 0, 1<<20), path: f.Name()}, nil
}

func (s *spillFile) appendHash(h [32]byte) error {
	const recLen = 32
	const flushAt = (1 << 20) - recLen
	if len(s.buf)+recLen > flushAt {
		if _, err := s.f.Write(s.buf); err != nil {
			return err
		}
		s.buf = s.buf[:0]
	}
	s.buf = append(s.buf, h[:]...)
	s.count++
	return nil
}

func (s *spillFile) flush() error {
	if len(s.buf) > 0 {
		if _, err := s.f.Write(s.buf); err != nil {
			return err
		}
		s.buf = s.buf[:0]
	}
	return s.f.Sync()
}

func (s *spillFile) cleanup() {
	if s == nil || s.f == nil {
		return
	}
	_ = s.flush()
	_ = s.f.Close()
	_ = os.Remove(s.path)
}

// aggregateHistogram reads the per-slot address spill, computes the bucketed
// histogram of contracts-by-slot-count, and produces the slot seeds the
// tracker needs. Memory is bounded by the count of unique addresses per
// nibble-pass (16 passes over the spill).
func aggregateHistogram(path string, log zerolog.Logger) (
	hist [16]int64, contractsWithStorage int64, seeds []tracker.SlotSeed, err error) {
	f, err := os.Open(path)
	if err != nil {
		return hist, 0, nil, err
	}
	defer f.Close()

	const passes = 16
	for p := 0; p < passes; p++ {
		hi := byte(p << 4)
		mask := byte(0xF0)
		counts := make(map[[32]byte]int64, 1<<20)
		if _, err := f.Seek(0, 0); err != nil {
			return hist, 0, nil, err
		}
		buf := make([]byte, 32*(1<<16))
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				n -= n % 32
				for i := 0; i < n; i += 32 {
					if buf[i]&mask != hi {
						continue
					}
					var a [32]byte
					copy(a[:], buf[i:i+32])
					counts[a]++
				}
			}
			if rerr != nil {
				break
			}
		}
		for a, c := range counts {
			if c <= 0 {
				continue
			}
			bucket := log2Bucket(c)
			if bucket >= 16 {
				bucket = 15
			}
			hist[bucket]++
			contractsWithStorage++
			seeds = append(seeds, tracker.SlotSeed{HashedAddress: a, SlotCount: uint64(c)})
		}
		log.Debug().Int("pass", p+1).Int("unique", len(counts)).Msg("histogram pass")
	}
	return hist, contractsWithStorage, seeds, nil
}

func log2Bucket(slotCount int64) int {
	n := uint64(slotCount + 1)
	bucket := 0
	for n > 1 {
		n >>= 1
		bucket++
	}
	return bucket
}

// buildCodeOpts builds the RocksDB Options + Cache used to open the Code DB
// for read-only iteration during the streaming dedup merge-join.
func buildCodeOpts(blockCacheMiB int, useMmap bool) (*grocksdb.Options, *grocksdb.Cache) {
	if blockCacheMiB <= 0 {
		blockCacheMiB = 128
	}
	cache := grocksdb.NewLRUCache(uint64(blockCacheMiB) * 1024 * 1024)
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(cache)
	opts := grocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(false)
	opts.SetMaxOpenFiles(128)
	opts.SetAllowMmapReads(useMmap)
	return opts, cache
}

// drainDedupSeedsStreaming streams the codehash spill through the dedup
// RocksDB and pushes one CodeSeed (refcount=1, codeSize=0) per unique
// codehash to sink as soon as it is read from the dedup DB iterator. Used
// by the --skip-code path so the tracker still observes the correct
// uniqueCodeHashes cardinality even without the Code DB lookup.
//
// Memory is bounded by the dedup DB's write-buffer × maxWriteBufferNumber,
// not by the number of unique entries. No []CodeSeed slice is ever
// materialised — the earlier slice-building wrapper was removed because
// at bloatnet scale (~1.37 B unique codehashes) the slice alone took
// ~60 GB and OOM'd the bootstrap.
func drainDedupSeedsStreaming(
	spillPath, dedupDir string,
	log zerolog.Logger,
	sink func(tracker.CodeSeed) error,
) (int64, error) {
	dedupDB, cleanup, _, err := streamSpillIntoDedupDB(spillPath, dedupDir, log)
	if err != nil {
		return 0, fmt.Errorf("dedup ingest: %w", err)
	}
	defer cleanup()

	ro := grocksdb.NewDefaultReadOptions()
	defer ro.Destroy()
	ro.SetFillCache(false)
	ro.SetReadaheadSize(2 * 1024 * 1024)
	it := dedupDB.NewIterator(ro)
	defer it.Close()

	var unique int64
	for it.SeekToFirst(); it.Valid(); it.Next() {
		k := it.Key()
		if k.Size() == 32 {
			var h [32]byte
			copy(h[:], k.Data())
			if serr := sink(tracker.CodeSeed{CodeHash: h, Refcount: 1}); serr != nil {
				k.Free()
				return unique, fmt.Errorf("dedup drain sink: %w", serr)
			}
			unique++
		}
		k.Free()
	}
	if ierr := it.Err(); ierr != nil {
		return unique, fmt.Errorf("dedup drain: %w", ierr)
	}
	return unique, nil
}
