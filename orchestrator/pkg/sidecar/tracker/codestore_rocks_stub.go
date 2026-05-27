//go:build nogrocksdb

package tracker

import "errors"

// RocksCodeStore is the disk-backed CodeStore. Under the nogrocksdb build tag
// it is compiled out; callers attempting to instantiate one get an explicit
// error so the failure mode is loud rather than silently falling back to RAM.
type RocksCodeStore struct{}

// OpenRocksCodeStore returns an error under the nogrocksdb build tag.
func OpenRocksCodeStore(dir string) (*RocksCodeStore, error) {
	return nil, errors.New("RocksCodeStore unavailable: rebuild without -tags nogrocksdb")
}

func (s *RocksCodeStore) Get(hash [32]byte) (codeEntry, bool) { return codeEntry{}, false }
func (s *RocksCodeStore) MultiGet(hashes [][32]byte) (results []codeEntry, present []bool) {
	return make([]codeEntry, len(hashes)), make([]bool, len(hashes))
}
func (s *RocksCodeStore) Put(hash [32]byte, e codeEntry)                 {}
func (s *RocksCodeStore) BatchPut(hashes [][32]byte, entries []codeEntry) {}
func (s *RocksCodeStore) Delete(hash [32]byte)                           {}
func (s *RocksCodeStore) Iterate(fn func(hash [32]byte, e codeEntry) bool) error {
	return nil
}
func (s *RocksCodeStore) Len() int64            { return 0 }
func (s *RocksCodeStore) Close() error          { return nil }
func (s *RocksCodeStore) addCount(int64)        {}
func (s *RocksCodeStore) SetBulkMode(bool)      {}
func (s *RocksCodeStore) FlushAfterBulk() error { return nil }

// BatchIngest stub.
type BatchIngest struct{}

func (s *RocksCodeStore) NewBatchIngest(int) *BatchIngest { return &BatchIngest{} }
func (b *BatchIngest) Add(hash [32]byte, e codeEntry) error {
	return errors.New("BatchIngest unavailable under nogrocksdb")
}
func (b *BatchIngest) Close() error { return nil }
func (b *BatchIngest) Added() int64 { return 0 }
