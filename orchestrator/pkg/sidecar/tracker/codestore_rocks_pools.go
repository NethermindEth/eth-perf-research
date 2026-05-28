//go:build !nogrocksdb

package tracker

import (
	"sync"

	"github.com/linxGnu/grocksdb"
)

// rocksScratch holds the [][]byte slice that MultiGet hands to grocksdb.
// The slice headers and the backing array are pooled across calls so a
// steady-state bloating loop doesn't reallocate them per block.
type rocksScratch struct {
	keys [][]byte
}

var rocksScratchPool = sync.Pool{
	New: func() any { return &rocksScratch{keys: make([][]byte, 0, 256)} },
}

func acquireRocksScratch() *rocksScratch { return rocksScratchPool.Get().(*rocksScratch) }
func releaseRocksScratch(s *rocksScratch) { rocksScratchPool.Put(s) }

// writeBatchPool reuses grocksdb.WriteBatch instances across BatchPut
// calls. The Put list grows as the WriteBatch is used; Clear() on
// release resets it without freeing the C-side memory.
var writeBatchPool = sync.Pool{
	New: func() any { return grocksdb.NewWriteBatch() },
}

func acquireWriteBatch() *grocksdb.WriteBatch { return writeBatchPool.Get().(*grocksdb.WriteBatch) }
func releaseWriteBatch(wb *grocksdb.WriteBatch) {
	wb.Clear()
	writeBatchPool.Put(wb)
}
