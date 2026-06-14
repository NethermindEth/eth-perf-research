//go:build nogrocksdb

// Stub for the nogrocksdb build; lets cmd/sidecar and tests compile without
// requiring CGO/rocksdb on the developer's machine.
package scanner

import (
	"context"
	"errors"

	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/db"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// Options mirrors the real type so callers compile.
type Options struct {
	SpillDir     string
	SkipCode     bool
	CodeDBPath   string
	BlockCache   int
	UseMmap      bool
	DedupDir     string
	CodeStoreDir string
}

// Result mirrors the real type.
type Result struct {
	BlockNumber  int64
	StateRoot    [32]byte
	Tracker      *tracker.Tracker
	Counters     tracker.ScanCounters
	ElapsedMs    int64
	AccountNodes int64
	StorageNodes int64
}

// Run always errors in the stub build.
func Run(ctx context.Context, h *db.Handle, opts Options, log zerolog.Logger) (Result, error) {
	return Result{}, errors.New("scanner: built without grocksdb")
}
