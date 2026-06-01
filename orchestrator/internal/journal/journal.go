// Package journal provides a length-prefixed protobuf binlog with a BLAKE3
// chain hash over each record's ReplayCore sub-message.
//
// Wire format: [4-byte big-endian length][protobuf-serialised Record] ...
//
// Chain hash: BLAKE3(prev_chain_hash || canonical_replay_core_bytes)
// where canonical_replay_core_bytes is the deterministic proto encoding of
// ReplayCore with ChainHash cleared to nil/empty before hashing.
package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"google.golang.org/protobuf/proto"
	"lukechampine.com/blake3"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// ErrChainHashMismatch is returned when the stored chain_hash in a record
// does not match the locally re-derived BLAKE3 hash.
var ErrChainHashMismatch = errors.New("journal: chain-hash mismatch")

var zeroHash [32]byte

// computeChainHash derives BLAKE3(prevHash || canonical_replay_core_bytes).
// The record's ChainHash field must be cleared by the caller before calling.
func computeChainHash(prev [32]byte, rc *orchpb.ReplayCore) ([32]byte, error) {
	coreBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(rc)
	if err != nil {
		return zeroHash, fmt.Errorf("journal: marshal replay_core for hashing: %w", err)
	}
	h := blake3.New(32, nil)
	h.Write(prev[:])
	h.Write(coreBytes)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func writeFramed(f *os.File, data []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := f.Write(hdr[:]); err != nil {
		return fmt.Errorf("journal: write length header: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("journal: write payload: %w", err)
	}
	return nil
}

func readFramed(f *os.File) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, err // may be io.EOF or io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("journal: read payload (%d bytes): %w", n, err)
	}
	return buf, nil
}

// Writer appends records to a journal file.  Single-writer invariant — callers
// must not share a Writer across goroutines without external serialisation.
type Writer struct {
	mu       sync.Mutex
	f        *os.File
	prevHash [32]byte
}

// OpenWriter opens (or creates) a journal file for appending and positions the
// internal chain-hash state at the tail of whatever records already exist.
func OpenWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open writer %q: %w", path, err)
	}
	w := &Writer{f: f}
	// Fast-forward to derive the current tail hash without keeping records in memory.
	_, tailHash, err := tailFromFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	w.prevHash = tailHash
	// Position at end for future appends.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, fmt.Errorf("journal: seek to end: %w", err)
	}
	return w, nil
}

// Append computes the chain hash, writes it into rec.ReplayCore.ChainHash,
// marshals the record, writes the framed bytes and fsyncs.
// Returns the new chain hash.  The caller's record is mutated.
func (w *Writer) Append(rec *orchpb.Record) ([32]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if rec.ReplayCore == nil {
		rec.ReplayCore = &orchpb.ReplayCore{}
	}

	// Clear chain_hash before hashing so the hash covers the other fields only.
	rec.ReplayCore.ChainHash = nil
	newHash, err := computeChainHash(w.prevHash, rec.ReplayCore)
	if err != nil {
		return zeroHash, err
	}
	rec.ReplayCore.ChainHash = newHash[:]

	data, err := proto.Marshal(rec)
	if err != nil {
		return zeroHash, fmt.Errorf("journal: marshal record: %w", err)
	}
	if err := writeFramed(w.f, data); err != nil {
		return zeroHash, err
	}
	if err := w.f.Sync(); err != nil {
		return zeroHash, fmt.Errorf("journal: fsync: %w", err)
	}
	w.prevHash = newHash
	return newHash, nil
}

func (w *Writer) CurrentChainHash() [32]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prevHash
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// Reader reads records sequentially from a journal file, verifying chain hashes.
type Reader struct {
	f        *os.File
	prevHash [32]byte
}

func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journal: open reader %q: %w", path, err)
	}
	return &Reader{f: f}, nil
}

// Next reads and verifies the next record.
// Returns io.EOF on a clean end-of-file.
// Returns ErrChainHashMismatch if the embedded chain_hash doesn't match.
func (r *Reader) Next() (*orchpb.Record, error) {
	buf, err := readFramed(r.f)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("journal: read frame: %w", err)
	}

	rec := new(orchpb.Record)
	if err := proto.Unmarshal(buf, rec); err != nil {
		return nil, fmt.Errorf("journal: unmarshal record: %w", err)
	}

	if rec.ReplayCore == nil {
		rec.ReplayCore = &orchpb.ReplayCore{}
	}
	storedHash := make([]byte, len(rec.ReplayCore.ChainHash))
	copy(storedHash, rec.ReplayCore.ChainHash)

	rec.ReplayCore.ChainHash = nil
	derived, err := computeChainHash(r.prevHash, rec.ReplayCore)
	if err != nil {
		return nil, err
	}
	rec.ReplayCore.ChainHash = storedHash

	if len(storedHash) != 32 {
		return nil, fmt.Errorf("%w: stored hash has length %d, want 32", ErrChainHashMismatch, len(storedHash))
	}
	var stored32 [32]byte
	copy(stored32[:], storedHash)
	if stored32 != derived {
		return nil, ErrChainHashMismatch
	}

	r.prevHash = derived
	return rec, nil
}

func (r *Reader) Close() error {
	return r.f.Close()
}

// tailFromFile reads f from the start, returning the last record and final
// chain hash.  Returns (nil, zeroHash, nil) for an empty file.
// The file position is left at an unspecified location after the call.
func tailFromFile(f *os.File) (*orchpb.Record, [32]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, zeroHash, fmt.Errorf("journal: seek start: %w", err)
	}
	var (
		prev    [32]byte
		lastRec *orchpb.Record
	)
	for {
		buf, err := readFramed(f)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, zeroHash, fmt.Errorf("journal: read frame in tail scan: %w", err)
		}
		rec := new(orchpb.Record)
		if err := proto.Unmarshal(buf, rec); err != nil {
			return nil, zeroHash, fmt.Errorf("journal: unmarshal in tail scan: %w", err)
		}
		if rec.ReplayCore == nil {
			rec.ReplayCore = &orchpb.ReplayCore{}
		}
		if len(rec.ReplayCore.ChainHash) == 32 {
			copy(prev[:], rec.ReplayCore.ChainHash)
		}
		lastRec = rec
	}
	return lastRec, prev, nil
}

// Tail returns the last record in path and its chain hash.
// Returns (nil, 32-zero-hash, nil) for an empty file.
func Tail(path string) (*orchpb.Record, [32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, zeroHash, fmt.Errorf("journal: tail open %q: %w", path, err)
	}
	defer f.Close()
	rec, hash, err := tailFromFile(f)
	return rec, hash, err
}
