// BlockDiffRecord wire format (canonical, written by Phase 2 NM plugin).
//
// Key:   binary.BigEndian.AppendUint64(nil, blockNumber)   — 8 bytes.
// Value: RLP-encoded list of items, in this order:
//
//	[ blockNumber:uint64,
//	  stateRoot:[32]byte,
//	  codeHashChanges:[]CodeHashChange,
//	  slotCountChanges:[]SlotCountChange ]
//
//	CodeHashChange  = [ oldHash:[32]byte, newHash:[32]byte, newCodeSize:uint64 ]
//	SlotCountChange = [ hashedAddress:[32]byte, oldCount:uint64, newCount:uint64 ]
//
// The sidecar interprets oldHash == NoCode (all-zero [32]byte) as "account had
// no code before this block"; newHash == NoCode means "account dropped its
// code". The same sentinel rules apply as in Nethermind.StateComposition.Data.
// CodeHashChange (`NoCode = default(ValueHash256)`).
//
// SlotCountChange.{Old,New}Count are the absolute slot counts at parent and
// new state roots respectively. The sidecar applies (NewCount - OldCount)
// to its slot-total and re-buckets the contract in the slot-count histogram.
package rlp

import (
	"fmt"
	"io"

	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// CodeHashChange encodes a per-account code-hash transition for one block.
type CodeHashChange struct {
	OldHash     [32]byte
	NewHash     [32]byte
	NewCodeSize uint64
}

// HadCode reports whether the account carried code before this block.
func (c CodeHashChange) HadCode() bool { return c.OldHash != ZeroHash }

// HasCode reports whether the account carries code after this block.
func (c CodeHashChange) HasCode() bool { return c.NewHash != ZeroHash }

// SlotCountChange encodes a per-contract net change in storage slot count.
type SlotCountChange struct {
	HashedAddress [32]byte
	OldCount      uint64
	NewCount      uint64
}

// Delta returns NewCount - OldCount as a signed int64.
func (s SlotCountChange) Delta() int64 {
	return int64(s.NewCount) - int64(s.OldCount)
}

// BlockDiffRecord is the value half of the BlockDiffs CF entry.
// AccountTrieBytesDelta / StorageTrieBytesDelta / AccountsAddedDelta are
// trailing additive fields (NM plugin PR-A wire-format v2). Legacy
// payloads end after SlotCountChanges; the decoder defaults the trailing
// trio to 0 in that case. Signed: net new bytes / accounts (negative on
// SELFDESTRUCT pre-Cancun).
type BlockDiffRecord struct {
	BlockNumber           uint64
	StateRoot             [32]byte
	CodeHashChanges       []CodeHashChange
	SlotCountChanges      []SlotCountChange
	AccountTrieBytesDelta int64
	StorageTrieBytesDelta int64
	AccountsAddedDelta    int64
}

// ZeroHash is the NoCode sentinel — matches Nethermind's
// CodeHashChange.NoCode (default(ValueHash256)).
var ZeroHash = [32]byte{}

// EncodeBlockDiff serialises a BlockDiffRecord to its canonical RLP value.
// Used by tests and by the integration test's synthetic NM plugin emulator.
func EncodeBlockDiff(rec BlockDiffRecord) ([]byte, error) {
	return gethrlp.EncodeToBytes(toWire(rec))
}

// DecodeBlockDiff parses an RLP-encoded BlockDiffRecord (the CF value).
// Tolerates legacy payloads (4 fields, schema v1) and PR-A v2 payloads
// (7 fields with the trailing trio). Missing trailing fields default to 0.
//
// Uses rlp.SplitList + rlp.Split which iterate the outer-list items by byte
// boundary — robust to extra trailing fields without explicit ListEnd.
func DecodeBlockDiff(buf []byte) (BlockDiffRecord, error) {
	listKind, listContent, _, err := gethrlp.Split(buf)
	if err != nil {
		return BlockDiffRecord{}, fmt.Errorf("decode BlockDiffRecord: split outer: %w", err)
	}
	if listKind != gethrlp.List {
		return BlockDiffRecord{}, fmt.Errorf("decode BlockDiffRecord: outer not a list")
	}

	items, err := splitAll(listContent)
	if err != nil {
		return BlockDiffRecord{}, err
	}
	if len(items) < 4 {
		return BlockDiffRecord{}, fmt.Errorf("decode BlockDiffRecord: want >=4 fields, got %d", len(items))
	}

	var rec BlockDiffRecord
	if err := gethrlp.DecodeBytes(items[0], &rec.BlockNumber); err != nil {
		return rec, fmt.Errorf("decode blockNumber: %w", err)
	}
	var root []byte
	if err := gethrlp.DecodeBytes(items[1], &root); err != nil {
		return rec, fmt.Errorf("decode stateRoot: %w", err)
	}
	if len(root) != 0 {
		if len(root) != 32 {
			return rec, fmt.Errorf("stateRoot len=%d, want 32", len(root))
		}
		copy(rec.StateRoot[:], root)
	}
	if rec.CodeHashChanges, err = decodeCodeChangesPayload(items[2]); err != nil {
		return rec, err
	}
	if rec.SlotCountChanges, err = decodeSlotChangesPayload(items[3]); err != nil {
		return rec, err
	}
	if len(items) >= 5 {
		v, err := decodeUint64Lenient(items[4])
		if err != nil {
			return rec, fmt.Errorf("decode AccountTrieBytesDelta: %w", err)
		}
		rec.AccountTrieBytesDelta = int64(v)
	}
	if len(items) >= 6 {
		v, err := decodeUint64Lenient(items[5])
		if err != nil {
			return rec, fmt.Errorf("decode StorageTrieBytesDelta: %w", err)
		}
		rec.StorageTrieBytesDelta = int64(v)
	}
	if len(items) >= 7 {
		v, err := decodeUint64Lenient(items[6])
		if err != nil {
			return rec, fmt.Errorf("decode AccountsAddedDelta: %w", err)
		}
		rec.AccountsAddedDelta = int64(v)
	}
	return rec, nil
}

// decodeUint64Lenient reads an RLP integer payload into a uint64, accepting
// non-canonical encodings (e.g. 8-byte big-endian regardless of value size,
// which is how Nethermind's RlpStream.Encode(long) writes negative values
// as two's-complement). The bit pattern is preserved verbatim so the caller
// can int64-cast to recover the signed value.
//
// Uses gethrlp.SplitString, NOT Split: go-ethereum's RLP returns Kind=Byte
// (not String) for single-byte values 0x00-0x7f. A previous version checked
// `kind != String` and so rejected every delta in [1,127] with "not a
// string-encoded integer" — those blocks were silently skipped by the
// tailer, dropping their byte deltas and drifting the incremental tracker
// away from ground truth (only a full bootstrap rescan corrected it).
// SplitString accepts both Byte and String content and rejects only List,
// which is exactly the lenient contract we need.
func decodeUint64Lenient(itemRLP []byte) (uint64, error) {
	content, _, err := gethrlp.SplitString(itemRLP)
	if err != nil {
		return 0, err
	}
	if len(content) > 8 {
		return 0, fmt.Errorf("integer >8 bytes")
	}
	var v uint64
	for _, b := range content {
		v = (v << 8) | uint64(b)
	}
	return v, nil
}

// splitAll iterates the items in an RLP list payload and returns their raw
// (kind+content) encodings as a slice of byte-spans.
func splitAll(payload []byte) ([][]byte, error) {
	var out [][]byte
	for len(payload) > 0 {
		_, _, rest, err := gethrlp.Split(payload)
		if err != nil {
			return nil, fmt.Errorf("split item: %w", err)
		}
		itemLen := len(payload) - len(rest)
		out = append(out, payload[:itemLen])
		payload = rest
	}
	return out, nil
}

func decodeCodeChangesPayload(itemRLP []byte) ([]CodeHashChange, error) {
	kind, content, _, err := gethrlp.Split(itemRLP)
	if err != nil {
		return nil, fmt.Errorf("decode CodeHashChanges: split: %w", err)
	}
	if kind != gethrlp.List {
		return nil, fmt.Errorf("decode CodeHashChanges: not a list")
	}
	entries, err := splitAll(content)
	if err != nil {
		return nil, err
	}
	out := make([]CodeHashChange, 0, len(entries))
	for i, entryRLP := range entries {
		ek, ec, _, err := gethrlp.Split(entryRLP)
		if err != nil {
			return nil, fmt.Errorf("decode CodeHashChange[%d]: split: %w", i, err)
		}
		if ek != gethrlp.List {
			return nil, fmt.Errorf("decode CodeHashChange[%d]: not a list", i)
		}
		fields, err := splitAll(ec)
		if err != nil {
			return nil, err
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("decode CodeHashChange[%d]: want 3 fields, got %d", i, len(fields))
		}
		var c CodeHashChange
		var oh, nh []byte
		if err := gethrlp.DecodeBytes(fields[0], &oh); err != nil {
			return nil, err
		}
		if err := gethrlp.DecodeBytes(fields[1], &nh); err != nil {
			return nil, err
		}
		if err := assertHashLen(oh, "oldHash"); err != nil {
			return nil, err
		}
		if err := assertHashLen(nh, "newHash"); err != nil {
			return nil, err
		}
		if len(oh) == 32 {
			copy(c.OldHash[:], oh)
		}
		if len(nh) == 32 {
			copy(c.NewHash[:], nh)
		}
		if err := gethrlp.DecodeBytes(fields[2], &c.NewCodeSize); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func decodeSlotChangesPayload(itemRLP []byte) ([]SlotCountChange, error) {
	kind, content, _, err := gethrlp.Split(itemRLP)
	if err != nil {
		return nil, fmt.Errorf("decode SlotCountChanges: split: %w", err)
	}
	if kind != gethrlp.List {
		return nil, fmt.Errorf("decode SlotCountChanges: not a list")
	}
	entries, err := splitAll(content)
	if err != nil {
		return nil, err
	}
	out := make([]SlotCountChange, 0, len(entries))
	for i, entryRLP := range entries {
		ek, ec, _, err := gethrlp.Split(entryRLP)
		if err != nil {
			return nil, fmt.Errorf("decode SlotCountChange[%d]: split: %w", i, err)
		}
		if ek != gethrlp.List {
			return nil, fmt.Errorf("decode SlotCountChange[%d]: not a list", i)
		}
		fields, err := splitAll(ec)
		if err != nil {
			return nil, err
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("decode SlotCountChange[%d]: want 3 fields, got %d", i, len(fields))
		}
		var c SlotCountChange
		var ha []byte
		if err := gethrlp.DecodeBytes(fields[0], &ha); err != nil {
			return nil, err
		}
		if err := assertHashLen(ha, "hashedAddress"); err != nil {
			return nil, err
		}
		if len(ha) == 32 {
			copy(c.HashedAddress[:], ha)
		}
		if err := gethrlp.DecodeBytes(fields[1], &c.OldCount); err != nil {
			return nil, err
		}
		if err := gethrlp.DecodeBytes(fields[2], &c.NewCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// --- Internal wire structs (the go-ethereum rlp package needs []byte fields,
//     not [32]byte arrays, to round-trip canonically). ---

type codeHashChangeWire struct {
	OldHash     []byte
	NewHash     []byte
	NewCodeSize uint64
}

type slotCountChangeWire struct {
	HashedAddress []byte
	OldCount      uint64
	NewCount      uint64
}

type blockDiffWire struct {
	BlockNumber      uint64
	StateRoot        []byte
	CodeHashChanges  []codeHashChangeWire
	SlotCountChanges []slotCountChangeWire
}

func toWire(rec BlockDiffRecord) blockDiffWire {
	codes := make([]codeHashChangeWire, len(rec.CodeHashChanges))
	for i, c := range rec.CodeHashChanges {
		oh := c.OldHash
		nh := c.NewHash
		codes[i] = codeHashChangeWire{
			OldHash:     oh[:],
			NewHash:     nh[:],
			NewCodeSize: c.NewCodeSize,
		}
	}
	slots := make([]slotCountChangeWire, len(rec.SlotCountChanges))
	for i, s := range rec.SlotCountChanges {
		ha := s.HashedAddress
		slots[i] = slotCountChangeWire{
			HashedAddress: ha[:],
			OldCount:      s.OldCount,
			NewCount:      s.NewCount,
		}
	}
	root := rec.StateRoot
	return blockDiffWire{
		BlockNumber:      rec.BlockNumber,
		StateRoot:        root[:],
		CodeHashChanges:  codes,
		SlotCountChanges: slots,
	}
}

func fromWire(w blockDiffWire) (BlockDiffRecord, error) {
	rec := BlockDiffRecord{BlockNumber: w.BlockNumber}
	if len(w.StateRoot) != 0 {
		if len(w.StateRoot) != 32 {
			return BlockDiffRecord{}, fmt.Errorf("stateRoot len=%d, want 32", len(w.StateRoot))
		}
		copy(rec.StateRoot[:], w.StateRoot)
	}
	if len(w.CodeHashChanges) > 0 {
		rec.CodeHashChanges = make([]CodeHashChange, len(w.CodeHashChanges))
		for i, c := range w.CodeHashChanges {
			if err := assertHashLen(c.OldHash, "oldHash"); err != nil {
				return BlockDiffRecord{}, err
			}
			if err := assertHashLen(c.NewHash, "newHash"); err != nil {
				return BlockDiffRecord{}, err
			}
			rec.CodeHashChanges[i].NewCodeSize = c.NewCodeSize
			if len(c.OldHash) == 32 {
				copy(rec.CodeHashChanges[i].OldHash[:], c.OldHash)
			}
			if len(c.NewHash) == 32 {
				copy(rec.CodeHashChanges[i].NewHash[:], c.NewHash)
			}
		}
	}
	if len(w.SlotCountChanges) > 0 {
		rec.SlotCountChanges = make([]SlotCountChange, len(w.SlotCountChanges))
		for i, s := range w.SlotCountChanges {
			if err := assertHashLen(s.HashedAddress, "hashedAddress"); err != nil {
				return BlockDiffRecord{}, err
			}
			rec.SlotCountChanges[i].OldCount = s.OldCount
			rec.SlotCountChanges[i].NewCount = s.NewCount
			if len(s.HashedAddress) == 32 {
				copy(rec.SlotCountChanges[i].HashedAddress[:], s.HashedAddress)
			}
		}
	}
	return rec, nil
}

// assertHashLen permits a zero-length string (canonical RLP encoding of the
// all-zero NoCode sentinel) and a full 32-byte hash. Anything else is data
// corruption — bubble it up so the tailer can refuse to apply.
func assertHashLen(b []byte, name string) error {
	if len(b) == 0 || len(b) == 32 {
		return nil
	}
	return fmt.Errorf("%s: invalid length %d, want 0 or 32", name, len(b))
}

// DecodeBlockDiffStream decodes a single record from an io.Reader. Used by
// snapshot.Reader on the side; kept here so the canonical RLP layout lives
// in one file.
func DecodeBlockDiffStream(r io.Reader) (BlockDiffRecord, error) {
	var w blockDiffWire
	if err := gethrlp.Decode(r, &w); err != nil {
		return BlockDiffRecord{}, fmt.Errorf("decode BlockDiffRecord (stream): %w", err)
	}
	return fromWire(w)
}
