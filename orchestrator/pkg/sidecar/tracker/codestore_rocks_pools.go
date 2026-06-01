//go:build !nogrocksdb

package tracker

import (
	"sync"

	"github.com/linxGnu/grocksdb"
)

// rocksScratch holds the [][]byte slice for MultiGet; pooled to avoid
// per-block reallocation.
type rocksScratch struct {
	keys [][]byte
}

var rocksScratchPool = sync.Pool{
	New: func() any { return &rocksScratch{keys: make([][]byte, 0, 256)} },
}

func acquireRocksScratch() *rocksScratch  { return rocksScratchPool.Get().(*rocksScratch) }
func releaseRocksScratch(s *rocksScratch) { rocksScratchPool.Put(s) }

// writeBatchPool reuses WriteBatch instances. Clear() on release resets
// the Put list without freeing the C-side memory.
var writeBatchPool = sync.Pool{
	New: func() any { return grocksdb.NewWriteBatch() },
}

func acquireWriteBatch() *grocksdb.WriteBatch { return writeBatchPool.Get().(*grocksdb.WriteBatch) }
func releaseWriteBatch(wb *grocksdb.WriteBatch) {
	wb.Clear()
	writeBatchPool.Put(wb)
}
