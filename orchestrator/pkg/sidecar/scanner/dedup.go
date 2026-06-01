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

// streamSpillIntoDedupDB opens a fresh RocksDB at dedupDir, ingests every
// 32-byte record from the spill via batched Puts, then flushes and compacts.
// On success the returned db is open for read iteration. The caller must invoke
// the returned cleanup to close the db and remove dedupDir.
func streamSpillIntoDedupDB(spillPath, dedupDir string, log zerolog.Logger) (
	db *grocksdb.DB,
	cleanup func(),
	written int64,
	err error,
) {
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

	f, ferr := os.Open(spillPath)
	if ferr != nil {
		cleanup()
		return nil, nil, 0, fmt.Errorf("open spill %s: %w", spillPath, ferr)
	}
	defer f.Close()

	wo := grocksdb.NewDefaultWriteOptions()
	wo.DisableWAL(true) // temp DB: durability not needed
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

	// Flush and compact so the merge-join iterator sees a fully-sorted layout.
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

// streamingDedupAndLookupSink runs the dedup+merge-join pipeline and pushes
// one (codehash, codeSize, refcount=1) seed to sink per unique codehash.
// Memory is bounded by the RocksDB write-buffer + block-cache (~1.5 GB total).
//
// Refcount=1 is intentional: contractsTotal is counted separately in the
// account-CF scan; using the actual refcount would double-count code reuse.
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

	codeOpts, codeCache := buildCodeOpts(codeDBBlockCacheMiB, useMmap)
	defer codeOpts.Destroy()
	defer codeCache.Destroy()

	codeDB, oerr := grocksdb.OpenDbForReadOnly(codeOpts, codeDBPath, false)
	if oerr != nil {
		return 0, 0, 0, 0, fmt.Errorf("open code db %s: %w", codeDBPath, oerr)
	}
	defer codeDB.Close()

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

	// Skip non-32-byte keys defensively; stock NM uses 32-byte codehash keys
	// but other CFs/tools might leave stray entries.
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
			dk.Free()
			ck.Free()
			dedupIt.Next()
			continue
		}
		cmp := bytes.Compare(dk.Data(), ck.Data())

		switch {
		case cmp == 0:
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
			// Referenced but absent in codeDB — data integrity issue.
			missingInCodeDB++
			uniqueCount++
			dk.Free()
			ck.Free()
			dedupIt.Next()
		default:
			// Present in codeDB but not referenced — orphaned (archive-mode leftover).
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
