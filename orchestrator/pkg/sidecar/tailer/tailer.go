//go:build !nogrocksdb

// Package tailer drives incremental tracker updates by reading the BlockDiffs
// column family in block-number order.
//
// The CF schema is fixed by pkg/rlp/diffs.go: keys are 8-byte big-endian
// uint64 block numbers, values are RLP-encoded BlockDiffRecords.
package tailer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/db"
	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// ErrNoBlockDiffsCF is returned when the FlatDb does not yet have a BlockDiffs
// column family — i.e. the Phase 2 NM plugin is not deployed. The caller can
// treat this as a soft warning in `tail` mode (sleep, retry, log).
var ErrNoBlockDiffsCF = errors.New("tailer: BlockDiffs CF not present")

// Tailer applies BlockDiffs CF records to the in-memory tracker.
type Tailer struct {
	h           *db.Handle
	t           *tracker.Tracker
	log         zerolog.Logger
	pollEvery   time.Duration
	lastApplied int64
}

// New constructs a Tailer. pollEvery is the dwell time between CatchUp/scan iterations.
func New(h *db.Handle, t *tracker.Tracker, log zerolog.Logger, pollEvery time.Duration) *Tailer {
	if pollEvery <= 0 {
		pollEvery = time.Second
	}
	return &Tailer{h: h, t: t, log: log, pollEvery: pollEvery, lastApplied: t.LastBlock()}
}

// Run blocks until ctx is cancelled. Each tick: (1) catch up secondary WAL,
// (2) iterate BlockDiffs from key > lastApplied, (3) apply to tracker.
func (l *Tailer) Run(ctx context.Context) error {
	if l.h.BlockDiffsCF == nil {
		l.log.Warn().Msg("tailer: BlockDiffs CF not present, idling")
	}

	ticker := time.NewTicker(l.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := l.tick(ctx); err != nil {
				l.log.Error().Err(err).Msg("tailer tick failed")
			}
		}
	}
}

func (l *Tailer) tick(ctx context.Context) error {
	if l.h.BlockDiffsCF == nil {
		return nil // idle until the CF appears
	}
	if err := l.h.CatchUp(); err != nil {
		l.log.Warn().Err(err).Msg("tailer: CatchUp failed")
	}
	return l.consumeFrom(ctx, l.lastApplied+1)
}

// consumeFrom seeks to the first block-number key >= start and applies every
// subsequent record to the tracker. Returns when the iterator hits EOF for
// this iteration (the next tick will resume from lastApplied+1).
func (l *Tailer) consumeFrom(ctx context.Context, start int64) error {
	if start < 0 {
		start = 0
	}
	ro := db.NewScanReadOptions()
	defer ro.Destroy()

	it := l.h.DB.NewIteratorCF(ro, l.h.BlockDiffsCF)
	defer it.Close()

	startKey := make([]byte, 8)
	binary.BigEndian.PutUint64(startKey, uint64(start))
	it.Seek(startKey)

	applied := 0
	for ; it.Valid(); it.Next() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		k := it.Key()
		v := it.Value()
		key := k.Data()
		val := v.Data()
		if len(key) != 8 {
			k.Free()
			v.Free()
			l.log.Warn().Int("len", len(key)).Msg("tailer: skipping non-8-byte BlockDiffs key")
			continue
		}
		blockNum := int64(binary.BigEndian.Uint64(key))
		// Copy before Next(): RocksDB may recycle the slice on the next call.
		valCopy := append([]byte(nil), val...)
		k.Free()
		v.Free()

		rec, err := rlppkg.DecodeBlockDiff(valCopy)
		if err != nil {
			l.log.Error().Err(err).Int64("block", blockNum).Msg("tailer: decode failed, skipping")
			l.lastApplied = blockNum
			// Advance the tracker's block pointer even on decode failure so
			// statecomp_lite doesn't stall when the failed block is the chain head.
			l.t.AdvanceLastBlock(blockNum)
			continue
		}
		if rec.BlockNumber == 0 {
			rec.BlockNumber = uint64(blockNum) // tolerate header-elided wire
		}
		l.t.ApplyBlockDiff(rec)
		l.lastApplied = blockNum
		applied++
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("tailer iter: %w", err)
	}
	if applied > 0 {
		l.log.Debug().Int("applied", applied).Int64("last_block", l.lastApplied).Msg("tailer applied")
	}
	return nil
}

// LastApplied returns the highest block-number key applied to the tracker.
func (l *Tailer) LastApplied() int64 { return l.lastApplied }

// EncodeKey is the canonical big-endian uint64 key encoder for BlockDiffs.
func EncodeKey(blockNumber uint64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, blockNumber)
	return out
}
