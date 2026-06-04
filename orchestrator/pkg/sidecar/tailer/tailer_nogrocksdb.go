//go:build nogrocksdb

package tailer

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/db"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// ErrNoBlockDiffsCF mirrors the real package's sentinel.
var ErrNoBlockDiffsCF = errors.New("tailer: BlockDiffs CF not present")

// Tailer is a no-op in the stub build; Run blocks until ctx is cancelled.
type Tailer struct {
	t           *tracker.Tracker
	log         zerolog.Logger
	lastApplied int64
}

func New(h *db.Handle, t *tracker.Tracker, log zerolog.Logger, pollEvery time.Duration) *Tailer {
	return &Tailer{t: t, log: log, lastApplied: t.LastBlock()}
}

// SetHeadSink is a no-op in the stub build (no CF to read a head from).
func (l *Tailer) SetHeadSink(fn func(int64)) {}

func (l *Tailer) Run(ctx context.Context) error {
	l.log.Warn().Msg("tailer (stub): no rocksdb; idling")
	<-ctx.Done()
	return ctx.Err()
}

// LastApplied returns the highest block number applied so far.
func (l *Tailer) LastApplied() int64 { return l.lastApplied }

// EncodeKey mirrors the real implementation for test use.
func EncodeKey(blockNumber uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, blockNumber)
	return out
}
