//go:build !nogrocksdb

package db

import "github.com/linxGnu/grocksdb"

// NewScanReadOptions returns ReadOptions configured for bulk sequential scans:
// no fill_cache (we'll evict L0 anyway after each pass) and aggressive
// readahead so HDDs can stream sequentially.
func NewScanReadOptions() *grocksdb.ReadOptions {
	ro := grocksdb.NewDefaultReadOptions()
	ro.SetFillCache(false)
	ro.SetReadaheadSize(2 * 1024 * 1024)
	return ro
}
