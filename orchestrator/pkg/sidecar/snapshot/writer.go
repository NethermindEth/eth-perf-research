// Package snapshot writes and reads the sidecar's tracker-state blob.
//
// Layout (binary, zstd-compressed):
//
//	magic    [4]byte   = "S19\x01"     // "Sidecar v19, schema 1"
//	header   varuint64 length-prefixed JSON header (sidecar metadata)
//	trackerKeys: repeat:
//	  section: varuint8 type (0 = end, 1 = code, 2 = slot)
//	  count:   varuint64
//	  records: count × (40 bytes slot | 44 bytes code)
//
// Atomic publish: write to <path>.new, fsync, rename → <path>. Same two-phase
// pattern as Nethermind.StateComposition.Snapshots.StateCompositionSnapshotStore.
package snapshot

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// Magic identifies a sidecar snapshot blob.
var Magic = [4]byte{'S', '1', '9', 0x01}

const (
	secCode = byte(1)
	secSlot = byte(2)
	secEnd  = byte(0)
)

// Header is the JSON header serialised before the per-key sections.
type Header struct {
	SchemaVersion int    `json:"schemaVersion"`
	WrittenAt     string `json:"writtenAt"`
	BlockNumber   int64  `json:"blockNumber"`
	StateRoot     string `json:"stateRoot"`

	// Tier-2 scan-only counters (copied verbatim out of the tracker).
	AccountsTotal         int64 `json:"accountsTotal"`
	EmptyAccounts         int64 `json:"emptyAccounts"`
	AccountTrieBranches   int64 `json:"accountTrieBranches"`
	AccountTrieExtensions int64 `json:"accountTrieExtensions"`
	AccountTrieLeaves     int64 `json:"accountTrieLeaves"`
	AccountTrieBytes      int64 `json:"accountTrieBytes"`
	StorageTrieBranches   int64 `json:"storageTrieBranches"`
	StorageTrieExtensions int64 `json:"storageTrieExtensions"`
	StorageTrieLeaves     int64 `json:"storageTrieLeaves"`
	StorageTrieBytes      int64 `json:"storageTrieBytes"`

	SlotHistogram []int64 `json:"slotHistogram"`

	// CodesExternal, when true, signals that the per-codehash records were
	// NOT serialised into this snapshot blob — the writer's tracker was
	// backed by a persistent (disk) CodeStore which is the authoritative
	// source. Readers must reopen that store to rebuild the aggregate
	// counters. False on a writer with an in-memory CodeStore (the
	// snapshot.bin then carries every code seed and is fully
	// self-contained).
	CodesExternal bool `json:"codesExternal,omitempty"`

	// ShardCodeCounters carries the per-shard aggregate code counters so
	// RestoreInto can skip the multi-minute RebuildCountersFromCodeStore
	// walk on restart. Schema v2+. One entry per shard, in shard index
	// order. Length == tracker.ShardCount; missing/wrong-length triggers
	// the legacy rebuild path for backwards compat with v1 snapshots.
	ShardCodeCounters []tracker.ShardCodeCounter `json:"shardCodeCounters,omitempty"`
}

// Write atomically writes the tracker state to <dir>/snapshot.bin. Returns the
// final on-disk path. If `dir` does not exist it is created.
func Write(dir string, t *tracker.Tracker) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	final := filepath.Join(dir, "snapshot.bin")
	tmp := final + ".new"
	if err := writeFile(tmp, t); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename snapshot: %w", err)
	}
	return final, nil
}

func writeFile(path string, t *tracker.Tracker) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open snapshot tmp: %w", err)
	}
	defer func() {
		syncErr := f.Sync()
		closeErr := f.Close()
		if err == nil {
			err = syncErr
		}
		if err == nil {
			err = closeErr
		}
	}()

	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() {
		flushErr := bw.Flush()
		if err == nil {
			err = flushErr
		}
	}()

	if _, err := bw.Write(Magic[:]); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}

	// SpeedFastest: snapshot.bin is never shipped long-term; the ~30% size
	// penalty is worth the ~3× CPU reduction for this restart-only blob.
	enc, err := zstd.NewWriter(bw, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	defer func() {
		closeErr := enc.Close()
		if err == nil {
			err = closeErr
		}
	}()

	snap := t.SnapshotCounters()
	hdr := Header{
		SchemaVersion: 2,
		WrittenAt:     time.Now().UTC().Format(time.RFC3339Nano),
		BlockNumber:   snap.LastBlock,
		StateRoot: func() string {
			if snap.StateRoot == ([32]byte{}) {
				return ""
			}
			return "0x" + hex.EncodeToString(snap.StateRoot[:])
		}(),
		AccountsTotal:         snap.AccountsTotal,
		EmptyAccounts:         snap.EmptyAccounts,
		AccountTrieBranches:   snap.AccountTrieBranches,
		AccountTrieExtensions: snap.AccountTrieExtensions,
		AccountTrieLeaves:     snap.AccountTrieLeaves,
		AccountTrieBytes:      snap.AccountTrieBytes,
		StorageTrieBranches:   snap.StorageTrieBranches,
		StorageTrieExtensions: snap.StorageTrieExtensions,
		StorageTrieLeaves:     snap.StorageTrieLeaves,
		StorageTrieBytes:      snap.StorageTrieBytes,
		SlotHistogram:         append([]int64(nil), snap.SlotHistogram[:]...),
		CodesExternal:         t.CodesAreExternal(),
		ShardCodeCounters:     t.ExportShardCodeCounters(),
	}
	hdrBytes, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}
	if err := writeUvarint(enc, uint64(len(hdrBytes))); err != nil {
		return err
	}
	if _, err := enc.Write(hdrBytes); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	if err := writeShards(enc, t); err != nil {
		return err
	}
	return nil
}

func writeShards(w io.Writer, t *tracker.Tracker) error {
	// When CodeStore is disk-backed (CodesExternal), skip serialising code
	// records: 1.4 B entries × 30 s writes would saturate I/O for no gain.
	// The reader pulls counters from the reopened store via the header bit.
	external := t.CodesAreExternal()
	var codeCount int64
	if !external {
		n, err := countCodes(t)
		if err != nil {
			return fmt.Errorf("count codes: %w", err)
		}
		codeCount = n
	}
	slotCount := t.SlotEntryCount()

	// Code section.
	if err := writeByte(w, secCode); err != nil {
		return err
	}
	if err := writeUvarint(w, uint64(codeCount)); err != nil {
		return err
	}
	var emitErr error
	if !external {
		var cbuf [44]byte
		werr := t.ExportCodes(func(c tracker.CodeSeed) bool {
			copy(cbuf[0:32], c.CodeHash[:])
			binary.BigEndian.PutUint64(cbuf[32:40], c.CodeSize)
			binary.BigEndian.PutUint32(cbuf[40:44], c.Refcount)
			if _, werr2 := w.Write(cbuf[:]); werr2 != nil {
				emitErr = fmt.Errorf("write code seed: %w", werr2)
				return false
			}
			return true
		})
		if werr != nil {
			return fmt.Errorf("iterate codes: %w", werr)
		}
		if emitErr != nil {
			return emitErr
		}
	}

	// Slot section.
	if err := writeByte(w, secSlot); err != nil {
		return err
	}
	if err := writeUvarint(w, uint64(slotCount)); err != nil {
		return err
	}
	var sbuf [40]byte
	t.ExportSlots(func(s tracker.SlotSeed) bool {
		copy(sbuf[0:32], s.HashedAddress[:])
		binary.BigEndian.PutUint64(sbuf[32:40], s.SlotCount)
		if _, werr2 := w.Write(sbuf[:]); werr2 != nil {
			emitErr = fmt.Errorf("write slot seed: %w", werr2)
			return false
		}
		return true
	})
	if emitErr != nil {
		return emitErr
	}

	return writeByte(w, secEnd)
}

// countCodes iterates the CodeStore once to count entries. Used by writeShards
// to emit the section length up-front so readers can pre-size decode buffers.
func countCodes(t *tracker.Tracker) (int64, error) {
	var n int64
	err := t.ExportCodes(func(tracker.CodeSeed) bool {
		n++
		return true
	})
	return n, err
}

func writeByte(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}

func writeUvarint(w io.Writer, v uint64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], v)
	if _, err := w.Write(buf[:n]); err != nil {
		return fmt.Errorf("write uvarint: %w", err)
	}
	return nil
}
