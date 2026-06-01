package payloads

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
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
		LogsBloom:     bloom[:],
		Random:        randHash(rng),
		Number:        uint64(i + 1),
		GasLimit:      30_000_000,
		GasUsed:       uint64(rng.Intn(30_000_000)),
		Timestamp:     uint64(1_700_000_000 + i*12),
		ExtraData:     randBytes(rng, extraLen),
		BaseFeePerGas: new(big.Int).SetUint64(uint64(rng.Intn(1e10) + 1)),
		BlockHash:     randHash(rng),
		Transactions:  txs,
		Withdrawals:   ws,
		BlobGasUsed:   new(uint64),
		ExcessBlobGas: new(uint64),
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
	if err := os.WriteFile(path, []byte{0x00, 0x01}, 0o644); err != nil {
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

func TestAppendSyncReadable(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	path := filepath.Join(t.TempDir(), "sync.bin")

	w, err := OpenWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	want := randPayload(rng, 0)
	if err := w.Append(want); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// A second reader on the still-open writer must observe the synced frame.
	r, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}

	normalise(want)
	normalise(got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("mismatch after Append+Sync\nwant %+v\ngot  %+v", want, got)
	}
}

// TestRLPByteStabilityGolden asserts that a known payload still RLP-encodes
// to the exact byte sequence that prior consumers (sidecar, replay, external
// Geth replayers) expect. Any change to field order, type, or RLP layout will
// flip this hash. The expected hash was computed from the canonical wire
// representation locked at v1; do not update without a migration plan.
func TestRLPByteStabilityGolden(t *testing.T) {
	bloom := make([]byte, 256)
	for i := range bloom {
		bloom[i] = byte(i)
	}
	zero := uint64(0)
	p := &ExecutionPayloadV3{
		ParentHash:    common.Hash{0x01},
		FeeRecipient:  common.Address{0x02},
		StateRoot:     common.Hash{0x03},
		ReceiptsRoot:  common.Hash{0x04},
		LogsBloom:     bloom,
		Random:        common.Hash{0x05},
		Number:        100,
		GasLimit:      30_000_000,
		GasUsed:       21000,
		Timestamp:     1_700_000_000,
		ExtraData:     []byte{0xde, 0xad},
		BaseFeePerGas: big.NewInt(7),
		BlockHash:     common.Hash{0x06},
		Transactions:  [][]byte{{0xff}},
		Withdrawals:   []*types.Withdrawal{},
		BlobGasUsed:   &zero,
		ExcessBlobGas: &zero,
	}
	buf, err := rlp.EncodeToBytes(toRLP(p))
	if err != nil {
		t.Fatalf("rlp encode: %v", err)
	}
	got := sha256.Sum256(buf)
	const want = "99544c9cc8e2571aea1fc1cf7a3c8756f837a1591580b39032d961c51ce08bfc"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("wire format changed: got sha256=%x want %s (len=%d)", got[:], want, len(buf))
	}
}
