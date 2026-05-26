//go:build !nogrocksdb

package scanner

// Streaming code-hash dedup via a temporary RocksDB then merge-join with the
// code DB. Memory is bounded by RocksDB write-buffer × max-write-buffers
// regardless of unique codehash cardinality. Disk for the temp DB is ~1.5x
// the spill size (LSM overhead); caller can redirect via --dedup-dir.

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/linxGnu/grocksdb"
	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

const (
	dedupWriteBufferBytes  = 256 << 20 // 256 MB
	dedupMaxWriteBuffers   = 4
	dedupL0CompactTrigger  = 4
	dedupBloomBitsPerKey   = 10
	dedupTargetFileSize    = 256 << 20 // 256 MB
	dedupBlockCacheBytes   = 128 << 20 // 128 MB
	dedupWriteBatchRecords = 8192      // 8192 × 32 B = 256 KB per batch
)

// streamSpillIntoDedupDB opens a fresh RocksDB at dedupDir, streams every
// 32-byte record from spillPath into it via batched Puts, then flushes and
// full-range compacts. On success the returned db is open for read iteration;
// caller is responsible for invoking the returned cleanup, which closes the
// db, destroys options/cache, and removes dedupDir.
func streamSpillIntoDedupDB(spillPath, dedupDir string, log zerolog.Logger) (
	db *grocksdb.DB,
	cleanup func(),
	written int64,
	err error,
) {
	// Cleanup any prior dedup attempt at this path so OpenDb sees a fresh dir.
	if rerr := os.RemoveAll(dedupDir); rerr != nil {
		return nil, nil, 0, fmt.Errorf("remove stale dedup dir %s: %w", dedupDir, rerr)
	}
	if merr := os.MkdirAll(dedupDir, 0o755); merr != nil {
		return nil, nil, 0, fmt.Errorf("mkdir dedup dir %s: %w", dedupDir, merr)
	}

	cache := grocksdb.NewLRUCache(dedupBlockCacheBytes)
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(cache)
	bbto.SetCacheIndexAndFilterBlocks(true)
	bbto.SetFilterPolicy(grocksdb.NewBloomFilter(dedupBloomBitsPerKey))

	opts := grocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(true)
	opts.SetErrorIfExists(true)
	opts.SetWriteBufferSize(dedupWriteBufferBytes)
	opts.SetMaxWriteBufferNumber(dedupMaxWriteBuffers)
	opts.SetLevel0FileNumCompactionTrigger(dedupL0CompactTrigger)
	opts.SetTargetFileSizeBase(dedupTargetFileSize)
	opts.SetMaxOpenFiles(256)

	db, err = grocksdb.OpenDb(opts, dedupDir)
	if err != nil {
		opts.Destroy()
		cache.Destroy()
		return nil, nil, 0, fmt.Errorf("open dedup db at %s: %w", dedupDir, err)
	}

	cleanup = func() {
		db.Close()
		opts.Destroy()
		cache.Destroy()
		_ = os.RemoveAll(dedupDir)
	}

	// Stream the spill in chunked Puts.
	f, ferr := os.Open(spillPath)
	if ferr != nil {
		cleanup()
		return nil, nil, 0, fmt.Errorf("open spill %s: %w", spillPath, ferr)
	}
	defer f.Close()

	wo := grocksdb.NewDefaultWriteOptions()
	wo.DisableWAL(true) // temp DB; durability not required
	defer wo.Destroy()

	const readChunk = 32 * (1 << 16) // 2 MiB
	rbuf := make([]byte, readChunk)
	wb := grocksdb.NewWriteBatch()
	defer wb.Destroy()

	flushBatch := func() error {
		if wb.Count() == 0 {
			return nil
		}
		if werr := db.Write(wo, wb); werr != nil {
			return werr
		}
		wb.Clear()
		return nil
	}

	writeStart := time.Now()
	var lastLog time.Time
	for {
		n, rerr := f.Read(rbuf)
		if n > 0 {
			n -= n % 32
			for i := 0; i < n; i += 32 {
				wb.Put(rbuf[i:i+32], nil)
				written++
				if wb.Count() >= dedupWriteBatchRecords {
					if werr := flushBatch(); werr != nil {
						cleanup()
						return nil, nil, written, fmt.Errorf("dedup write batch: %w", werr)
					}
				}
			}
		}
		if rerr != nil {
			break
		}
		if time.Since(lastLog) > 30*time.Second {
			log.Info().
				Int64("records", written).
				Dur("elapsed", time.Since(writeStart)).
				Msg("dedup ingest progress")
			lastLog = time.Now()
		}
	}
	if werr := flushBatch(); werr != nil {
		cleanup()
		return nil, nil, written, fmt.Errorf("dedup final batch: %w", werr)
	}
	log.Info().
		Int64("records", written).
		Dur("elapsed", time.Since(writeStart)).
		Msg("dedup ingest done")

	// Flush memtables to SST and run a full compaction so the merge-join
	// iterator sees the dedup DB at its smallest, fully-sorted layout.
	flushOpts := grocksdb.NewDefaultFlushOptions()
	flushOpts.SetWait(true)
	defer flushOpts.Destroy()
	if ferr := db.Flush(flushOpts); ferr != nil {
		cleanup()
		return nil, nil, written, fmt.Errorf("dedup flush: %w", ferr)
	}
	compactStart := time.Now()
	db.CompactRange(grocksdb.Range{Start: nil, Limit: nil})
	log.Info().Dur("elapsed", time.Since(compactStart)).Msg("dedup compact done")

	return db, cleanup, written, nil
}

// streamingDedupAndLookupSink performs the dedup+merge-join pipeline and
// pushes every (codehash, codeSize, refcount=1) tuple through sink as soon
// as it is computed. Memory is bounded by RocksDB's write-buffer + block
// cache (~1.5 GB total) regardless of unique-codehash cardinality, AND no
// Go-slice ever holds all seeds.
//
// Refcount=1 mirrors the existing bootstrap convention: the per-account
// contractsTotal is counted separately in the account-CF scan, so we must
// NOT use the actual refcount here (it would double-count code reuse).
//
// dedupDir is the on-disk location of the temp RocksDB. It will be created
// fresh (any prior contents removed) and deleted on return.
func streamingDedupAndLookupSink(
	spillPath string,
	codeDBPath string,
	dedupDir string,
	codeDBBlockCacheMiB int,
	useMmap bool,
	log zerolog.Logger,
	sink func(tracker.CodeSeed) error,
) (
	codeBytes int64,
	hits int64,
	uniqueCount int64,
	missingInCodeDB int64,
	err error,
) {
	st, statErr := os.Stat(spillPath)
	if statErr != nil {
		return 0, 0, 0, 0, fmt.Errorf("stat spill %s: %w", spillPath, statErr)
	}
	log.Info().
		Str("spill", spillPath).
		Int64("records", st.Size()/32).
		Int64("size_mib", st.Size()/(1024*1024)).
		Str("dedup_dir", dedupDir).
		Str("code_db", codeDBPath).
		Msg("streaming dedup+lookup")

	dedupDB, dedupCleanup, _, derr := streamSpillIntoDedupDB(spillPath, dedupDir, log)
	if derr != nil {
		return 0, 0, 0, 0, fmt.Errorf("dedup ingest: %w", derr)
	}
	defer dedupCleanup()

	// --- Open codeDB read-only ---
	codeOpts, codeCache := buildCodeOpts(codeDBBlockCacheMiB, useMmap)
	defer codeOpts.Destroy()
	defer codeCache.Destroy()

	codeDB, oerr := grocksdb.OpenDbForReadOnly(codeOpts, codeDBPath, false)
	if oerr != nil {
		return 0, 0, 0, 0, fmt.Errorf("open code db %s: %w", codeDBPath, oerr)
	}
	defer codeDB.Close()

	// --- Two iterators, merge-join ---
	dro := grocksdb.NewDefaultReadOptions()
	defer dro.Destroy()
	dro.SetFillCache(false)
	dro.SetReadaheadSize(2 * 1024 * 1024)
	dedupIt := dedupDB.NewIterator(dro)
	defer dedupIt.Close()

	cro := grocksdb.NewDefaultReadOptions()
	defer cro.Destroy()
	cro.SetFillCache(false)
	cro.SetReadaheadSize(2 * 1024 * 1024)
	codeIt := codeDB.NewIterator(cro)
	defer codeIt.Close()

	dedupIt.SeekToFirst()
	codeIt.SeekToFirst()

	// Advance codeIt past any non-32-byte keys (defensive; stock NM uses
	// 32-byte codehash keys but other CFs/tools might leave artifacts).
	advanceCodeTo32 := func() bool {
		for codeIt.Valid() {
			k := codeIt.Key()
			ok := k.Size() == 32
			k.Free()
			if ok {
				return true
			}
			codeIt.Next()
		}
		return false
	}

	joinStart := time.Now()
	var lastLog time.Time
	for dedupIt.Valid() && advanceCodeTo32() {
		dk := dedupIt.Key()
		ck := codeIt.Key()
		if dk.Size() != 32 {
			// Defensive: skip malformed entries on the dedup side too.
			dk.Free()
			ck.Free()
			dedupIt.Next()
			continue
		}
		cmp := bytes.Compare(dk.Data(), ck.Data())

		switch {
		case cmp == 0:
			// Hit: this codehash is referenced AND present in codeDB.
			var h [32]byte
			copy(h[:], dk.Data())
			cv := codeIt.Value()
			size := uint64(cv.Size())
			codeBytes += int64(size)
			cv.Free()
			if serr := sink(tracker.CodeSeed{
				CodeHash: h,
				CodeSize: size,
				Refcount: 1,
			}); serr != nil {
				dk.Free()
				ck.Free()
				return codeBytes, hits, uniqueCount, missingInCodeDB,
					fmt.Errorf("merge-join sink: %w", serr)
			}
			hits++
			uniqueCount++
			dk.Free()
			ck.Free()
			dedupIt.Next()
			codeIt.Next()
		case cmp < 0:
			// Codehash referenced but missing in codeDB — data integrity flag.
			missingInCodeDB++
			uniqueCount++
			dk.Free()
			ck.Free()
			dedupIt.Next()
		default:
			// Codehash present in codeDB but not referenced by any account.
			// Orphaned blob (archive-mode leftover); skip.
			dk.Free()
			ck.Free()
			codeIt.Next()
		}

		if time.Since(lastLog) > 30*time.Second {
			log.Info().
				Int64("unique", uniqueCount).
				Int64("hits", hits).
				Int64("missing", missingInCodeDB).
				Int64("code_bytes", codeBytes).
				Msg("merge-join progress")
			lastLog = time.Now()
		}
	}
	// Drain any remaining dedup entries (codeDB exhausted or never had them).
	for dedupIt.Valid() {
		dk := dedupIt.Key()
		if dk.Size() == 32 {
			missingInCodeDB++
			uniqueCount++
		}
		dk.Free()
		dedupIt.Next()
	}

	if ierr := dedupIt.Err(); ierr != nil {
		return codeBytes, hits, uniqueCount, missingInCodeDB, fmt.Errorf("dedup iter: %w", ierr)
	}
	if ierr := codeIt.Err(); ierr != nil {
		return codeBytes, hits, uniqueCount, missingInCodeDB, fmt.Errorf("code iter: %w", ierr)
	}
	log.Info().
		Dur("elapsed", time.Since(joinStart)).
		Int64("unique", uniqueCount).
		Int64("hits", hits).
		Int64("missing", missingInCodeDB).
		Int64("code_bytes", codeBytes).
		Msg("merge-join done")
	return codeBytes, hits, uniqueCount, missingInCodeDB, nil
}
