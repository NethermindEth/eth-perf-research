package snapshot

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// Restore loads <dir>/snapshot.bin into a freshly constructed Tracker. Returns
// (tracker, header, nil) on success.
//
// On a missing file Restore returns ErrNotFound so callers can distinguish
// "first launch, run bootstrap" from "corrupted snapshot, abort".
//
// The tracker created here uses the default in-memory CodeStore. To restore
// into a disk-backed CodeStore (bloatnet path; mandatory above ~50 M
// codehashes) use RestoreInto, which lets the caller pre-open the persistent
// store before the per-key sections are decoded.
func Restore(dir string) (*tracker.Tracker, Header, error) {
	return RestoreInto(dir, nil)
}

// RestoreInto is like Restore but installs codes as the active CodeStore
// before any per-key section is decoded. Pass nil to use the in-memory default.
// When a persistent store is supplied its existing authoritative entries are
// used; snapshot seeds are no-ops but still decoded to rebuild aggregate counters.
func RestoreInto(dir string, codes tracker.CodeStore) (*tracker.Tracker, Header, error) {
	path := filepath.Join(dir, "snapshot.bin")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Header{}, ErrNotFound
		}
		return nil, Header{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return nil, Header{}, fmt.Errorf("read magic: %w", err)
	}
	if magic != Magic {
		return nil, Header{}, fmt.Errorf("bad magic %x", magic)
	}

	dec, err := zstd.NewReader(br)
	if err != nil {
		return nil, Header{}, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	hdrLen, err := readUvarint(dec)
	if err != nil {
		return nil, Header{}, fmt.Errorf("read header len: %w", err)
	}
	hdrBytes := make([]byte, hdrLen)
	if _, err := io.ReadFull(dec, hdrBytes); err != nil {
		return nil, Header{}, fmt.Errorf("read header body: %w", err)
	}
	var hdr Header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, Header{}, fmt.Errorf("unmarshal header: %w", err)
	}

	var t *tracker.Tracker
	if codes != nil {
		t = tracker.NewWithCodeStore(codes)
	} else {
		t = tracker.New()
	}
	for {
		secType, err := readByte(dec)
		if err != nil {
			return nil, Header{}, fmt.Errorf("read section type: %w", err)
		}
		if secType == secEnd {
			break
		}
		count, err := readUvarint(dec)
		if err != nil {
			return nil, Header{}, fmt.Errorf("read section count: %w", err)
		}
		switch secType {
		case secCode:
			// Stream into the tracker directly to avoid an O(N) seed slice.
			var buf [44]byte
			var cs tracker.CodeSeed
			for i := uint64(0); i < count; i++ {
				if _, err := io.ReadFull(dec, buf[:]); err != nil {
					return nil, Header{}, fmt.Errorf("read code seed %d: %w", i, err)
				}
				copy(cs.CodeHash[:], buf[0:32])
				cs.CodeSize = binary.BigEndian.Uint64(buf[32:40])
				cs.Refcount = binary.BigEndian.Uint32(buf[40:44])
				t.SeedOneCode(cs)
			}
		case secSlot:
			var buf [40]byte
			var ss tracker.SlotSeed
			for i := uint64(0); i < count; i++ {
				if _, err := io.ReadFull(dec, buf[:]); err != nil {
					return nil, Header{}, fmt.Errorf("read slot seed %d: %w", i, err)
				}
				copy(ss.HashedAddress[:], buf[0:32])
				ss.SlotCount = binary.BigEndian.Uint64(buf[32:40])
				t.SeedOneSlot(ss)
			}
		default:
			return nil, Header{}, fmt.Errorf("unknown section type %d", secType)
		}
	}

	// CodesExternal: the code section was empty. Schema v2+ carries per-shard
	// counters in the header to skip RebuildCountersFromCodeStore.
	// v1 or length-mismatched slice falls back to the rebuild path.
	if hdr.CodesExternal && codes != nil {
		installed := false
		if len(hdr.ShardCodeCounters) == tracker.ShardCount {
			installed = t.SetShardCodeCounters(hdr.ShardCodeCounters)
		}
		if !installed {
			if err := t.RebuildCountersFromCodeStore(); err != nil {
				return nil, Header{}, fmt.Errorf("rebuild counters from codestore: %w", err)
			}
		}
	}

	t.SetScanCounters(tracker.ScanCounters{
		BlockNumber:           hdr.BlockNumber,
		StateRoot:             parseStateRoot(hdr.StateRoot),
		AccountsTotal:         hdr.AccountsTotal,
		EmptyAccounts:         hdr.EmptyAccounts,
		AccountTrieBranches:   hdr.AccountTrieBranches,
		AccountTrieExtensions: hdr.AccountTrieExtensions,
		AccountTrieLeaves:     hdr.AccountTrieLeaves,
		AccountTrieBytes:      hdr.AccountTrieBytes,
		StorageTrieBranches:   hdr.StorageTrieBranches,
		StorageTrieExtensions: hdr.StorageTrieExtensions,
		StorageTrieLeaves:     hdr.StorageTrieLeaves,
		StorageTrieBytes:      hdr.StorageTrieBytes,
		SlotHistogram:         hdr.SlotHistogram,
	})
	t.SetLastBlock(hdr.BlockNumber, parseStateRoot(hdr.StateRoot))

	return t, hdr, nil
}

// ErrNotFound is returned by Restore when no snapshot file exists in <dir>.
var ErrNotFound = errors.New("snapshot: no snapshot file in directory")

// ReadHeader peeks at the JSON header without materialising the per-key
// sections. Useful for the state machine to decide whether to enter
// CATCHING_UP or BOOTSTRAP_SCAN.
func ReadHeader(dir string) (Header, error) {
	path := filepath.Join(dir, "snapshot.bin")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Header{}, ErrNotFound
		}
		return Header{}, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return Header{}, err
	}
	if magic != Magic {
		return Header{}, fmt.Errorf("bad magic %x", magic)
	}
	dec, err := zstd.NewReader(br)
	if err != nil {
		return Header{}, err
	}
	defer dec.Close()
	hdrLen, err := readUvarint(dec)
	if err != nil {
		return Header{}, err
	}
	hdrBytes := make([]byte, hdrLen)
	if _, err := io.ReadFull(dec, hdrBytes); err != nil {
		return Header{}, err
	}
	var hdr Header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return Header{}, err
	}
	return hdr, nil
}

func readByte(r io.Reader) (byte, error) {
	if br, ok := r.(io.ByteReader); ok {
		return br.ReadByte()
	}
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func readUvarint(r io.Reader) (uint64, error) {
	if br, ok := r.(io.ByteReader); ok {
		return binary.ReadUvarint(br)
	}
	return binary.ReadUvarint(byteReader{r})
}

type byteReader struct{ r io.Reader }

func (b byteReader) ReadByte() (byte, error) {
	var buf [1]byte
	_, err := io.ReadFull(b.r, buf[:])
	return buf[0], err
}

func parseStateRoot(s string) [32]byte {
	var out [32]byte
	if s == "" {
		return out
	}
	s = strings.TrimPrefix(s, "0x")
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return out
	}
	copy(out[:], raw)
	return out
}
