//go:build !nogrocksdb

// Package db wraps grocksdb for the FlatDb open/iterate patterns the sidecar
// uses. Secondary mode (live NM, replays WAL) and read-only mode (offline).
//
// Build tag: pass `-tags nogrocksdb` to build without CGO/RocksDB; that
// substitutes a no-op stub useful for unit tests on machines without the
// rocksdb library installed.
package db

import (
	"fmt"
	"os"

	"github.com/linxGnu/grocksdb"
)

// CFNames lists every column family the sidecar reads.
type CFNames struct {
	State      string
	Storage    string
	Metadata   string
	BlockDiffs string
}

// Handle bundles the open DB plus the CF handles the sidecar uses. Callers
// MUST call Close() to release everything.
type Handle struct {
	DB           *grocksdb.DB
	StateCF      *grocksdb.ColumnFamilyHandle
	StorageCF    *grocksdb.ColumnFamilyHandle
	MetadataCF   *grocksdb.ColumnFamilyHandle
	BlockDiffsCF *grocksdb.ColumnFamilyHandle

	handles      []*grocksdb.ColumnFamilyHandle
	opts         *grocksdb.Options
	cache        *grocksdb.Cache
	secondaryDir string
}

// OpenOptions are knobs the caller controls; the rest is fixed policy.
type OpenOptions struct {
	BlockCacheMiB int
	UseMmap       bool
	Secondary     bool
}

// OpenBlockDiffsDB opens the standalone BlockDiffs RocksDB written by the
// Nethermind StateDiffsWriter plugin. The plugin keeps BlockDiffs as a
// separate database (datadir/blockDiffs/) with two CFs: "Default" (per-block
// records) and "SlotCounts" (per-address running slot map). The sidecar only
// needs the Default CF to drive the tailer; it surfaces as BlockDiffsCF on
// the returned handle so existing tailer code is unchanged.
func OpenBlockDiffsDB(path string, oo OpenOptions) (*Handle, error) {
	opts, cache := buildOptions(oo.BlockCacheMiB, oo.UseMmap)
	listed, err := grocksdb.ListColumnFamilies(opts, path)
	if err != nil {
		opts.Destroy()
		cache.Destroy()
		return nil, fmt.Errorf("list blockdiffs cfs: %w", err)
	}

	cfOpts := make([]*grocksdb.Options, len(listed))
	for i := range cfOpts {
		cfOpts[i] = opts
	}

	var ddb *grocksdb.DB
	var handles []*grocksdb.ColumnFamilyHandle
	var secondaryDir string
	if oo.Secondary {
		dir, derr := os.MkdirTemp("", "sidecar-blockdiffs-secondary-")
		if derr != nil {
			opts.Destroy()
			cache.Destroy()
			return nil, fmt.Errorf("mk blockdiffs secondary dir: %w", derr)
		}
		secondaryDir = dir
		ddb, handles, err = grocksdb.OpenDbAsSecondaryColumnFamilies(
			opts, path, dir, listed, cfOpts)
	} else {
		ddb, handles, err = grocksdb.OpenDbForReadOnlyColumnFamilies(
			opts, path, listed, cfOpts, false)
	}
	if err != nil {
		if secondaryDir != "" {
			_ = os.RemoveAll(secondaryDir)
		}
		opts.Destroy()
		cache.Destroy()
		return nil, fmt.Errorf("open blockdiffs db: %w", err)
	}

	cfByName := make(map[string]*grocksdb.ColumnFamilyHandle, len(listed))
	for i, name := range listed {
		cfByName[name] = handles[i]
	}
	// NM plugin names the per-block CF "Default"; the C# BlockDiffsColumns enum
	// maps to the standard RocksDB "default" CF. Accept either spelling.
	bdCF := cfByName["Default"]
	if bdCF == nil {
		bdCF = cfByName["default"]
	}
	// nil bdCF means an empty/uninitialised DB; tailer will idle but the
	// handle is kept alive so secondary CatchUp keeps replaying the WAL.
	h := &Handle{
		DB:           ddb,
		BlockDiffsCF: bdCF,
		handles:      handles,
		opts:         opts,
		cache:        cache,
		secondaryDir: secondaryDir,
	}
	return h, nil
}

// Open opens a FlatDb with the named CFs. A missing BlockDiffs CF is non-fatal.
func Open(path string, cfNames CFNames, oo OpenOptions) (*Handle, error) {
	opts, cache := buildOptions(oo.BlockCacheMiB, oo.UseMmap)
	listed, err := grocksdb.ListColumnFamilies(opts, path)
	if err != nil {
		opts.Destroy()
		cache.Destroy()
		return nil, fmt.Errorf("list cfs: %w", err)
	}

	cfOpts := make([]*grocksdb.Options, len(listed))
	for i := range cfOpts {
		cfOpts[i] = opts
	}

	var db *grocksdb.DB
	var handles []*grocksdb.ColumnFamilyHandle
	var secondaryDir string
	if oo.Secondary {
		dir, derr := os.MkdirTemp("", "sidecar-secondary-")
		if derr != nil {
			opts.Destroy()
			cache.Destroy()
			return nil, fmt.Errorf("mk secondary dir: %w", derr)
		}
		secondaryDir = dir
		db, handles, err = grocksdb.OpenDbAsSecondaryColumnFamilies(
			opts, path, dir, listed, cfOpts)
	} else {
		db, handles, err = grocksdb.OpenDbForReadOnlyColumnFamilies(
			opts, path, listed, cfOpts, false)
	}
	if err != nil {
		if secondaryDir != "" {
			_ = os.RemoveAll(secondaryDir)
		}
		opts.Destroy()
		cache.Destroy()
		return nil, fmt.Errorf("open db: %w", err)
	}

	cfByName := make(map[string]*grocksdb.ColumnFamilyHandle, len(listed))
	for i, name := range listed {
		cfByName[name] = handles[i]
	}
	h := &Handle{
		DB:           db,
		StateCF:      cfByName[cfNames.State],
		StorageCF:    cfByName[cfNames.Storage],
		MetadataCF:   cfByName[cfNames.Metadata],
		BlockDiffsCF: cfByName[cfNames.BlockDiffs],
		handles:      handles,
		opts:         opts,
		cache:        cache,
		secondaryDir: secondaryDir,
	}
	return h, nil
}

// Close releases the DB, all CF handles, options, cache, and the secondary scratch directory.
func (h *Handle) Close() {
	if h == nil {
		return
	}
	for _, cf := range h.handles {
		if cf != nil {
			cf.Destroy()
		}
	}
	if h.DB != nil {
		h.DB.Close()
	}
	if h.opts != nil {
		h.opts.Destroy()
	}
	if h.cache != nil {
		h.cache.Destroy()
	}
	if h.secondaryDir != "" {
		_ = os.RemoveAll(h.secondaryDir)
	}
}

// CatchUp asks RocksDB to replay any WAL entries written by the primary since
// the last call. Only valid in secondary mode; a no-op in read-only mode.
func (h *Handle) CatchUp() error {
	if h == nil || h.DB == nil {
		return nil
	}
	if h.secondaryDir == "" {
		return nil
	}
	return h.DB.TryCatchUpWithPrimary()
}

func buildOptions(blockCacheMiB int, useMmap bool) (*grocksdb.Options, *grocksdb.Cache) {
	if blockCacheMiB <= 0 {
		blockCacheMiB = 128
	}
	cache := grocksdb.NewLRUCache(uint64(blockCacheMiB) * 1024 * 1024)
	bbto := grocksdb.NewDefaultBlockBasedTableOptions()
	bbto.SetBlockCache(cache)
	bbto.SetCacheIndexAndFilterBlocks(true)
	bbto.SetPinL0FilterAndIndexBlocksInCache(true)
	opts := grocksdb.NewDefaultOptions()
	opts.SetBlockBasedTableFactory(bbto)
	opts.SetCreateIfMissing(false)
	opts.SetMaxOpenFiles(128)
	opts.SetAllowMmapReads(useMmap)
	return opts, cache
}
