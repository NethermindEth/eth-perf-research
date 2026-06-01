//go:build nogrocksdb

// Stub package that compiles without CGO / RocksDB. Used for unit tests on
// machines without the rocksdb library installed. The scanner / tailer
// modules degrade gracefully: scanner.Run returns an error, tailer idles.
package db

import (
	"errors"
)

// Sentinel returned by every Open call when the stub backend is active.
var ErrStubBackend = errors.New("db: built without grocksdb (use -tags=\"\" to enable RocksDB)")

type CFNames struct {
	State      string
	Storage    string
	Metadata   string
	BlockDiffs string
}

type OpenOptions struct {
	BlockCacheMiB int
	UseMmap       bool
	Secondary     bool
}

// Handle is a placeholder; every method is a no-op.
type Handle struct {
	DB           any
	StateCF      any
	StorageCF    any
	MetadataCF   any
	BlockDiffsCF any
}

func Open(path string, cfNames CFNames, oo OpenOptions) (*Handle, error) {
	return nil, ErrStubBackend
}

// OpenBlockDiffsDB is a no-op stub; the sidecar's tailer mode falls back to
// the idle path when this returns the sentinel error.
func OpenBlockDiffsDB(path string, oo OpenOptions) (*Handle, error) {
	return nil, ErrStubBackend
}

func (h *Handle) Close() {}

func (h *Handle) CatchUp() error { return nil }

type ReadOptions struct{}

func (*ReadOptions) Destroy() {}

func NewScanReadOptions() *ReadOptions { return &ReadOptions{} }
