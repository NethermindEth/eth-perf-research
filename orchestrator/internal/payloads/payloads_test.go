package payloads

import (
	"io"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func randHash(rng *rand.Rand) common.Hash {
	var h common.Hash
	rng.Read(h[:])
	return h
}

func randAddr(rng *rand.Rand) common.Address {
	var a common.Address
	rng.Read(a[:])
	return a
}

func randPayload(rng *rand.Rand, i int) *ExecutionPayloadV3 {
	var bloom [256]byte
	rng.Read(bloom[:])

	// Include 0–3 withdrawals for variety.
	wCount := rng.Intn(4)
	ws := make([]*types.Withdrawal, wCount)
	for j := range ws {
		ws[j] = &types.Withdrawal{
			Index:     uint64(i*10 + j),
			Validator: uint64(j),
			Address:   randAddr(rng),
			Amount:    uint64(rng.Intn(1e6)),
		}
	}

	// Include 1–5 transactions.
	txCount := 1 + rng.Intn(5)
	txs := make([][]byte, txCount)
	for j := range txs {
		txs[j] = randBytes(rng, 20+rng.Intn(200))
	}

	extraLen := rng.Intn(33) // 0–32 bytes
	return &ExecutionPayloadV3{
		ParentHash:    randHash(rng),
		FeeRecipient:  randAddr(rng),
		StateRoot:     randHash(rng),
		ReceiptsRoot:  randHash(rng),
		LogsBloom:     bloom,
		PrevRandao:    randHash(rng),
		BlockNumber:   uint64(i + 1),
		GasLimit:      30_000_000,
		GasUsed:       uint64(rng.Intn(30_000_000)),
		Timestamp:     uint64(1_700_000_000 + i*12),
		ExtraData:     randBytes(rng, extraLen),
		BaseFeePerGas: new(big.Int).SetUint64(uint64(rng.Intn(1e10) + 1)),
		BlockHash:     randHash(rng),
		Transactions:  txs,
		Withdrawals:   ws,
		BlobGasUsed:   0,
		ExcessBlobGas: 0,
	}
}

// normalise replaces nil slices with empty slices so reflect.DeepEqual works
// correctly across round-trips (RLP decodes missing lists as nil or empty).
func normalise(p *ExecutionPayloadV3) {
	if p.ExtraData == nil {
		p.ExtraData = []byte{}
	}
	if p.Transactions == nil {
		p.Transactions = [][]byte{}
	}
	if p.Withdrawals == nil {
		p.Withdrawals = []*types.Withdrawal{}
	}
	if p.BaseFeePerGas == nil {
		p.BaseFeePerGas = new(big.Int)
	}
}

func TestRoundTrip50(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	originals := make([]*ExecutionPayloadV3, 50)
	for i := range originals {
		originals[i] = randPayload(rng, i)
	}

	path := filepath.Join(t.TempDir(), "payloads.bin")

	w, err := OpenWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range originals {
		if err := w.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for i, want := range originals {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("entry %d: Next: %v", i, err)
		}
		normalise(want)
		normalise(got)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("entry %d: mismatch\nwant %+v\ngot  %+v", i, want, got)
		}
	}
	// Confirm EOF after last entry.
	_, err = r.Next()
	if err != io.EOF {
		t.Fatalf("expected io.EOF after last entry, got: %v", err)
	}
}

func TestEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, err = r.Next()
	if err != io.EOF {
		t.Fatalf("expected io.EOF on empty file, got: %v", err)
	}
}

func TestTruncatedLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.bin")
	// Write only 2 bytes of the 4-byte length header.
	if err := os.WriteFile(path, []byte{0x00, 0x01}, 0644); err != nil {
		t.Fatal(err)
	}

	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, err = r.Next()
	if err == nil {
		t.Fatal("expected error for truncated length header, got nil")
	}
	if !containsTruncated(err.Error()) {
		t.Fatalf("expected 'truncated' in error, got: %v", err)
	}
}

func containsTruncated(s string) bool {
	for i := 0; i+9 <= len(s); i++ {
		if s[i:i+9] == "truncated" {
			return true
		}
	}
	return false
}
