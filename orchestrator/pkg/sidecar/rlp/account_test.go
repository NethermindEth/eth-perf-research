package rlp

import (
	"bytes"
	"testing"

	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

func TestClassifyBranch(t *testing.T) {
	items := make([][]byte, 17)
	for i := range items {
		items[i] = []byte{}
	}
	enc, err := gethrlp.EncodeToBytes(items)
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := Classify(enc)
	if kind != NodeBranch {
		t.Fatalf("expected Branch, got %v", kind)
	}
}

func TestClassifyExtension(t *testing.T) {
	items := [][]byte{{0x00, 0x12, 0x34}, {0xaa, 0xbb}}
	enc, err := gethrlp.EncodeToBytes(items)
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := Classify(enc)
	if kind != NodeExtension {
		t.Fatalf("expected Extension, got %v", kind)
	}
}

func TestClassifyLeaf(t *testing.T) {
	value := []byte{0x42, 0x42, 0x42}
	items := [][]byte{{0x20, 0xab, 0xcd}, value}
	enc, err := gethrlp.EncodeToBytes(items)
	if err != nil {
		t.Fatal(err)
	}
	kind, leafVal := Classify(enc)
	if kind != NodeLeaf {
		t.Fatalf("expected Leaf, got %v", kind)
	}
	if !bytes.Equal(leafVal, value) {
		t.Fatalf("leaf value mismatch: %x vs %x", leafVal, value)
	}
}

func TestClassifyLeafOddNibble(t *testing.T) {
	items := [][]byte{{0x3a}, {0x99}}
	enc, err := gethrlp.EncodeToBytes(items)
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := Classify(enc)
	if kind != NodeLeaf {
		t.Fatalf("expected Leaf (odd), got %v", kind)
	}
}

func TestClassifyInvalid(t *testing.T) {
	if kind, _ := Classify(nil); kind != NodeInvalid {
		t.Fatalf("expected Invalid for nil, got %v", kind)
	}
	if kind, _ := Classify([]byte{0x80}); kind != NodeInvalid {
		t.Fatalf("expected Invalid for non-list, got %v", kind)
	}
}

type testAcct struct {
	Nonce       uint64
	Balance     []byte
	StorageRoot []byte
	CodeHash    []byte
}

func TestDecodeAccountEmpty(t *testing.T) {
	a := testAcct{
		Nonce:       0,
		Balance:     nil,
		StorageRoot: EmptyStorageRoot[:],
		CodeHash:    EmptyCodeHash[:],
	}
	enc, err := gethrlp.EncodeToBytes(a)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := DecodeAccount(enc)
	if !ok {
		t.Fatal("DecodeAccount failed")
	}
	if !info.IsEmpty {
		t.Fatalf("expected IsEmpty=true, got %+v", info)
	}
	if info.HasCode {
		t.Fatalf("expected HasCode=false, got true")
	}
	if info.HasStorage {
		t.Fatalf("expected HasStorage=false, got true")
	}
}

func TestDecodeAccountWithCode(t *testing.T) {
	codeHash := [32]byte{0x11, 0x22, 0x33}
	a := testAcct{
		Nonce:       5,
		Balance:     []byte{0x10},
		StorageRoot: EmptyStorageRoot[:],
		CodeHash:    codeHash[:],
	}
	enc, err := gethrlp.EncodeToBytes(a)
	if err != nil {
		t.Fatal(err)
	}
	info, ok := DecodeAccount(enc)
	if !ok {
		t.Fatal("DecodeAccount failed")
	}
	if !info.HasCode {
		t.Fatalf("expected HasCode=true, got false")
	}
	if info.IsEmpty {
		t.Fatalf("expected IsEmpty=false, got true")
	}
	if info.CodeHash != codeHash {
		t.Fatalf("codeHash mismatch: %x vs %x", info.CodeHash, codeHash)
	}
}
