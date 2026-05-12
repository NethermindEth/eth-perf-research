package signer

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// hardhat default key #0
const testHexKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func makeTx(nonce uint64) *orchpb.TxIn {
	tip := new(big.Int).SetUint64(1e9)
	fee := new(big.Int).SetUint64(2e9)
	val := new(big.Int).SetUint64(0)
	to := make([]byte, 20)
	to[19] = 0x01
	return &orchpb.TxIn{
		ChainId:              1,
		Nonce:                nonce,
		MaxPriorityFeePerGas: tip.Bytes(),
		MaxFeePerGas:         fee.Bytes(),
		Gas:                  21000,
		To:                   to,
		Value:                val.Bytes(),
		Data:                 nil,
	}
}

func assertSignedRLP(t *testing.T, raw []byte, idx int) {
	t.Helper()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("tx[%d]: UnmarshalBinary: %v", idx, err)
	}
	// Re-marshal and verify hash round-trips.
	raw2, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("tx[%d]: MarshalBinary: %v", idx, err)
	}
	var tx2 types.Transaction
	if err := tx2.UnmarshalBinary(raw2); err != nil {
		t.Fatalf("tx[%d]: re-unmarshal: %v", idx, err)
	}
	if tx.Hash() != tx2.Hash() {
		t.Fatalf("tx[%d]: hash mismatch after round-trip", idx)
	}
}

func TestSignBatch_One(t *testing.T) {
	s, err := New(testHexKey)
	if err != nil {
		t.Fatal(err)
	}
	raws, err := s.SignBatch(context.Background(), []*orchpb.TxIn{makeTx(0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(raws) != 1 {
		t.Fatalf("expected 1 result, got %d", len(raws))
	}
	assertSignedRLP(t, raws[0], 0)
}

func TestSignBatch_Ten(t *testing.T) {
	s, err := New(testHexKey)
	if err != nil {
		t.Fatal(err)
	}
	txs := make([]*orchpb.TxIn, 10)
	for i := range txs {
		txs[i] = makeTx(uint64(i))
	}
	raws, err := s.SignBatch(context.Background(), txs)
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range raws {
		assertSignedRLP(t, raw, i)
	}
}

func TestSignBatch_Thousand(t *testing.T) {
	s, err := New(testHexKey)
	if err != nil {
		t.Fatal(err)
	}
	txs := make([]*orchpb.TxIn, 1000)
	for i := range txs {
		txs[i] = makeTx(uint64(i))
	}
	raws, err := s.SignBatch(context.Background(), txs)
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range raws {
		assertSignedRLP(t, raw, i)
	}
}

func TestSignBatch_CtxCancel(t *testing.T) {
	s, err := New(testHexKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling

	txs := make([]*orchpb.TxIn, 100)
	for i := range txs {
		txs[i] = makeTx(uint64(i))
	}
	_, err = s.SignBatch(ctx, txs)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestNew_InvalidKey(t *testing.T) {
	_, err := New("notahexkey")
	if err == nil {
		t.Fatal("expected error for invalid hex key")
	}
}

func TestNew_WithPrefix(t *testing.T) {
	// Ensure 0x prefix is stripped correctly.
	_, err := New(testHexKey)
	if err != nil {
		t.Fatalf("0x-prefixed key rejected: %v", err)
	}
}
