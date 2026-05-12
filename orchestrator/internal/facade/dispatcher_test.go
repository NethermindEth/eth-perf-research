package facade

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
)

// hardhat default key #0 — same key used in signer_test.go.
const testHexKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// fakeBuilder implements batchBuilder for tests. It returns a configurable
// response without spawning any subprocess.
type fakeBuilder struct {
	// txGas is the Gas field placed on every returned TxIn.
	txGas uint64
	// newSaltCursor is echoed back as BuildBatchResponse.NewSaltCursor.
	newSaltCursor uint64
	// errMsg, if non-empty, is returned as BuildBatchResponse.Error.
	errMsg string
}

func (f *fakeBuilder) Build(_ context.Context, req *orchpb.BuildBatchRequest) (*orchpb.BuildBatchResponse, error) {
	if f.errMsg != "" {
		return &orchpb.BuildBatchResponse{Error: f.errMsg}, nil
	}
	to := make([]byte, 20)
	to[19] = 0x01

	txs := make([]*orchpb.TxIn, req.Count)
	for i := range txs {
		txs[i] = &orchpb.TxIn{
			Gas:   f.txGas,
			To:    to,
			Value: []byte{0},
			Data:  nil,
		}
	}
	return &orchpb.BuildBatchResponse{
		Signables:     txs,
		NewSaltCursor: f.newSaltCursor,
	}, nil
}

// newTestDispatcher builds a Dispatcher backed by the given fakeBuilder and the
// real signer constructed from testHexKey.
func newTestDispatcher(t *testing.T, fb *fakeBuilder) (*Dispatcher, *Context) {
	t.Helper()
	s, err := signer.New(testHexKey)
	if err != nil {
		t.Fatalf("signer.New: %v", err)
	}
	d := &Dispatcher{Pool: fb, Signer: s}
	c := &Context{
		BaseAddress:          make([]byte, 20),
		Revision:             1,
		AddressStride:        1 << 40,
		ChainID:              1,
		GasLimit:             30_000_000,
		BlockGasLimit:        0, // no gas cap unless overridden
		AddressCursor:        0,
		SaltCursor:           0,
		MaxFeePerGas:         new(big.Int).SetUint64(2e9),
		MaxPriorityFeePerGas: new(big.Int).SetUint64(1e9),
		VerbGasFactors:       map[string]float64{},
	}
	return d, c
}

func makePlan(verb string, nMaxTxs int, deadlineBytes int) *controller.BatchPlan {
	return &controller.BatchPlan{
		Verb:          verb,
		DeadlineBytes: deadlineBytes,
		NMaxTxs:       nMaxTxs,
	}
}

// assertValidRLP checks that raw bytes parse as a valid EIP-1559 transaction.
func assertValidRLP(t *testing.T, raw []byte, idx int) {
	t.Helper()
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		t.Fatalf("tx[%d]: UnmarshalBinary: %v", idx, err)
	}
}

// TestDispatch_TenTxs verifies that a plan with NMaxTxs=10 yields 10 signed RLPs.
func TestDispatch_TenTxs(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: 42}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 10, 0 /* no deadline */)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.TxCount != 10 {
		t.Fatalf("TxCount: want 10, got %d", res.TxCount)
	}
	if len(res.SignedRLP) != 10 {
		t.Fatalf("len(SignedRLP): want 10, got %d", len(res.SignedRLP))
	}
	if len(res.TxRLPHashes) != 10 {
		t.Fatalf("len(TxRLPHashes): want 10, got %d", len(res.TxRLPHashes))
	}
	for i, raw := range res.SignedRLP {
		assertValidRLP(t, raw, i)
	}
}

// TestDispatch_DeadlineTrim verifies that a tight deadline_bytes causes trimming.
// Each tx has an estimated size of 200+0+0 = 200 bytes (no data, no access list).
// With deadline = 350, only the first tx (200 bytes) fits; the second would
// push accumulated to 400 which exceeds the deadline.
func TestDispatch_DeadlineTrim(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 5, 350 /* bytes */)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.TxCount != 1 {
		t.Fatalf("TxCount: want 1 (deadline trim), got %d", res.TxCount)
	}
}

// TestDispatch_CursorAdvance verifies the address cursor advances by the actual
// number of signed transactions.
func TestDispatch_CursorAdvance(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: 99}
	d, c := newTestDispatcher(t, fb)
	c.AddressCursor = 1000

	plan := makePlan("transfer", 5, 0)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := uint64(1000 + res.TxCount)
	if c.AddressCursor != want {
		t.Fatalf("AddressCursor: want %d, got %d", want, c.AddressCursor)
	}
	if res.NewCursor != want {
		t.Fatalf("Result.NewCursor: want %d, got %d", want, res.NewCursor)
	}
}

// TestDispatch_SaltCursorAdvance verifies SaltCursor is updated from the
// response's NewSaltCursor field.
func TestDispatch_SaltCursorAdvance(t *testing.T) {
	const wantSalt = uint64(777)
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: wantSalt}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("deploy", 3, 0)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if c.SaltCursor != wantSalt {
		t.Fatalf("SaltCursor: want %d, got %d", wantSalt, c.SaltCursor)
	}
	if res.NewSalt != wantSalt {
		t.Fatalf("Result.NewSalt: want %d, got %d", wantSalt, res.NewSalt)
	}
}

// TestDispatch_GasCapTrim verifies the 0.95×BlockGasLimit gas cap.
//
// BlockGasLimit=100_000 → ceiling = 95_000.
// Each tx has Gas=21_000.
//
//	tx0 cumulative=21_000  ≤ 95_000 → accept (forward-progress always keeps first)
//	tx1 cumulative=42_000  ≤ 95_000 → accept
//	tx2 cumulative=63_000  ≤ 95_000 → accept
//	tx3 cumulative=84_000  ≤ 95_000 → accept
//	tx4 cumulative=105_000 > 95_000 → stop
//
// Expected: 4 txs.
func TestDispatch_GasCapTrim(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	c.BlockGasLimit = 100_000

	plan := makePlan("transfer", 10, 0)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.TxCount != 4 {
		t.Fatalf("TxCount: want 4 (gas cap trim), got %d", res.TxCount)
	}
}

// TestDispatch_WorkerError verifies that a non-empty BuildBatchResponse.Error
// is surfaced as a Go error.
func TestDispatch_WorkerError(t *testing.T) {
	fb := &fakeBuilder{errMsg: "worker exploded"}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 3, 0)
	_, err := d.Dispatch(context.Background(), plan, c)
	if err == nil {
		t.Fatal("expected error from worker, got nil")
	}
}

// TestDispatch_NilPlan ensures a nil plan returns an error rather than panicking.
func TestDispatch_NilPlan(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	_, err := d.Dispatch(context.Background(), nil, c)
	if err == nil {
		t.Fatal("expected error for nil plan")
	}
}

// TestDispatch_NilContext ensures a nil Context returns an error rather than panicking.
func TestDispatch_NilContext(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, _ := newTestDispatcher(t, fb)
	plan := makePlan("transfer", 3, 0)
	_, err := d.Dispatch(context.Background(), plan, nil)
	if err == nil {
		t.Fatal("expected error for nil context")
	}
}

// TestDispatch_RLPBytesSum verifies Result.RLPBytes equals sum of signed RLP lengths.
func TestDispatch_RLPBytesSum(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 5, 0)
	res, err := d.Dispatch(context.Background(), plan, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var want uint64
	for _, raw := range res.SignedRLP {
		want += uint64(len(raw))
	}
	if res.RLPBytes != want {
		t.Fatalf("RLPBytes: want %d, got %d", want, res.RLPBytes)
	}
}
