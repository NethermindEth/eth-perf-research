package rlp

import (
	"bytes"
	"testing"
)

func TestBlockDiffRoundTrip(t *testing.T) {
	in := BlockDiffRecord{
		BlockNumber: 12345,
		StateRoot:   [32]byte{0xaa, 0xbb, 0xcc, 0xdd},
		CodeHashChanges: []CodeHashChange{
			{OldHash: [32]byte{}, NewHash: [32]byte{0x11, 0x22}, NewCodeSize: 256},
			{OldHash: [32]byte{0x33, 0x44}, NewHash: [32]byte{}, NewCodeSize: 0},
		},
		SlotCountChanges: []SlotCountChange{
			{HashedAddress: [32]byte{0xde, 0xad}, OldCount: 5, NewCount: 7},
			{HashedAddress: [32]byte{0xbe, 0xef}, OldCount: 100, NewCount: 0},
		},
	}
	buf, err := EncodeBlockDiff(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeBlockDiff(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.BlockNumber != in.BlockNumber {
		t.Errorf("blockNumber: got %d want %d", out.BlockNumber, in.BlockNumber)
	}
	if !bytes.Equal(out.StateRoot[:], in.StateRoot[:]) {
		t.Errorf("stateRoot mismatch")
	}
	if len(out.CodeHashChanges) != len(in.CodeHashChanges) {
		t.Fatalf("codeHashChanges len: got %d want %d", len(out.CodeHashChanges), len(in.CodeHashChanges))
	}
	for i := range in.CodeHashChanges {
		if out.CodeHashChanges[i] != in.CodeHashChanges[i] {
			t.Errorf("codeHashChange[%d]: got %+v want %+v", i, out.CodeHashChanges[i], in.CodeHashChanges[i])
		}
	}
	if len(out.SlotCountChanges) != len(in.SlotCountChanges) {
		t.Fatalf("slotCountChanges len: got %d want %d", len(out.SlotCountChanges), len(in.SlotCountChanges))
	}
	for i := range in.SlotCountChanges {
		if out.SlotCountChanges[i] != in.SlotCountChanges[i] {
			t.Errorf("slotCountChange[%d]: got %+v want %+v", i, out.SlotCountChanges[i], in.SlotCountChanges[i])
		}
	}
}

func TestSlotCountChangeDelta(t *testing.T) {
	cases := []struct {
		old, new uint64
		want     int64
	}{
		{0, 100, 100},
		{50, 30, -20},
		{1, 1, 0},
	}
	for _, c := range cases {
		got := SlotCountChange{OldCount: c.old, NewCount: c.new}.Delta()
		if got != c.want {
			t.Errorf("Delta(%d, %d) = %d, want %d", c.old, c.new, got, c.want)
		}
	}
}

func TestCodeHashChangeHadHas(t *testing.T) {
	zero := [32]byte{}
	nz := [32]byte{0x01}
	c := CodeHashChange{OldHash: zero, NewHash: nz}
	if c.HadCode() {
		t.Errorf("HadCode should be false for zero old hash")
	}
	if !c.HasCode() {
		t.Errorf("HasCode should be true for nonzero new hash")
	}
}
