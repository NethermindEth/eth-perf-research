package facade

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// hardhat default key #0 — same key used in signer_test.go.
const testHexKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// fakeBuilder is a test verb with a configurable per-tx gas and an optional
// build error. It substitutes the native verb registry in dispatcher tests.
type fakeBuilder struct {
	txGas  uint64
	errMsg string
}

func (f *fakeBuilder) Name() string { return "transfer" }

func (f *fakeBuilder) BuildTx(_ uint64, _ verbs.BuildCtx) (*types.DynamicFeeTx, error) {
	if f.errMsg != "" {
		return nil, errors.New(f.errMsg)
	}
	to := common.Address{}
	to[19] = 0x01
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  nil,
		Gas:   f.txGas,
	}, nil
}

// newTestDispatcher builds a Dispatcher whose verb registry resolves every
// verb name to fb, backed by the real signer constructed from testHexKey.
func newTestDispatcher(t *testing.T, fb *fakeBuilder) (*Dispatcher, *Context) {
	t.Helper()
	s, err := signer.New(testHexKey)
	if err != nil {
		t.Fatalf("signer.New: %v", err)
	}
	d := &Dispatcher{
		Lookup: func(string) (verbs.Verb, bool) { return fb, true },
		Signer: s,
	}
	c := &Context{
		BaseAddress:   make([]byte, 20),
		Revision:      1,
		AddressStride: 1 << 40,
		ChainID:       1,
	}
	// BlockGasLimit defaults to zero (no gas cap unless tests override).
	c.SetFeePolicy(new(big.Int).SetUint64(2e9), new(big.Int).SetUint64(1e9))
	return d, c
}

func makePlan(verb string, nMaxTxs int, deadlineBytes int) *controller.BatchPlan {
	return &controller.BatchPlan{
		Verb:          verb,
		DeadlineBytes: deadlineBytes,
		NMaxTxs:       nMaxTxs,
	}
}

// inputFor reserves the plan's nonce/salt range from c and returns the
// corresponding DispatchInput. Mirrors the caller pattern in lifecycle.
func inputFor(plan *controller.BatchPlan, c *Context) DispatchInput {
	n := uint64(plan.NMaxTxs)
	if n == 0 {
		n = 1
	}
	return DispatchInput{
		Plan:       plan,
		StartNonce: c.ReserveAddresses(n),
		StartSalt:  c.ReserveSalts(n),
		NumNonces:  n,
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
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 10, 0 /* no deadline */)
	res, err := d.Dispatch(context.Background(), inputFor(plan, c), c)
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

// TestDispatch_CursorAdvance verifies the result's NewCursor reflects the
// actual number of signed transactions, computed from the reserved StartNonce.
func TestDispatch_CursorAdvance(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	c.AddressCursor.Store(1000)

	plan := makePlan("transfer", 5, 0)
	in := inputFor(plan, c)
	startNonce := in.StartNonce
	res, err := d.Dispatch(context.Background(), in, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := startNonce + uint64(res.TxCount)
	if res.NewCursor != want {
		t.Fatalf("Result.NewCursor: want %d, got %d", want, res.NewCursor)
	}
	// The atomic was advanced by the reservation, not by Dispatch — verify
	// it equals start + NumNonces (5 here).
	if got := c.LoadAddressCursor(); got != startNonce+5 {
		t.Fatalf("LoadAddressCursor after reserve: want %d, got %d", startNonce+5, got)
	}
}

// TestDispatch_SaltCursorAdvance verifies the returned NewSalt equals the
// caller-supplied StartSalt + NumNonces.
func TestDispatch_SaltCursorAdvance(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("deploy", 3, 0)
	in := inputFor(plan, c)
	res, err := d.Dispatch(context.Background(), in, c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := in.StartSalt + in.NumNonces
	if res.NewSalt != want {
		t.Fatalf("Result.NewSalt: want %d (StartSalt+NumNonces), got %d", want, res.NewSalt)
	}
}

// TestDispatch_GasCapError verifies that exceeding the 0.95×BlockGasLimit cap
// is a hard error rather than a silent trim. Under parallel planners, silent
// trimming would create nonce gaps: a reservation of N txs that is reduced to
// M<N leaves [StartNonce+M, StartNonce+N) holes which would stall the chain.
//
// BlockGasLimit=100_000 → ceiling = 95_000. Five txs at 21_000 gas each
// cumulate to 105_000 > 95_000, so Dispatch must return an error.
func TestDispatch_GasCapError(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	c.SetBlockGasLimit(100_000)

	plan := makePlan("transfer", 5, 0)
	_, err := d.Dispatch(context.Background(), inputFor(plan, c), c)
	if err == nil {
		t.Fatal("expected gas cap error, got nil")
	}
}

// TestDispatch_GasCapUnderBudget verifies that a plan within the gas budget
// dispatches successfully.
func TestDispatch_GasCapUnderBudget(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	c.SetBlockGasLimit(100_000)

	plan := makePlan("transfer", 4, 0) // 4×21k = 84k ≤ 95k
	res, err := d.Dispatch(context.Background(), inputFor(plan, c), c)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.TxCount != 4 {
		t.Fatalf("TxCount: want 4, got %d", res.TxCount)
	}
}

// TestDispatch_WorkerError verifies that a verb BuildTx error is surfaced as
// a Go error from Dispatch.
func TestDispatch_WorkerError(t *testing.T) {
	fb := &fakeBuilder{errMsg: "verb exploded"}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 3, 0)
	_, err := d.Dispatch(context.Background(), inputFor(plan, c), c)
	if err == nil {
		t.Fatal("expected error from worker, got nil")
	}
}

// TestDispatch_NilPlan ensures a nil plan returns an error rather than panicking.
func TestDispatch_NilPlan(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)
	_, err := d.Dispatch(context.Background(), DispatchInput{Plan: nil}, c)
	if err == nil {
		t.Fatal("expected error for nil plan")
	}
}

// TestDispatch_NilContext ensures a nil Context returns an error rather than panicking.
func TestDispatch_NilContext(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, _ := newTestDispatcher(t, fb)
	plan := makePlan("transfer", 3, 0)
	_, err := d.Dispatch(context.Background(), DispatchInput{Plan: plan, NumNonces: 3}, nil)
	if err == nil {
		t.Fatal("expected error for nil context")
	}
}

// TestDispatch_RLPBytesSum verifies Result.RLPBytes equals sum of signed RLP lengths.
func TestDispatch_RLPBytesSum(t *testing.T) {
	fb := &fakeBuilder{txGas: 21_000}
	d, c := newTestDispatcher(t, fb)

	plan := makePlan("transfer", 5, 0)
	res, err := d.Dispatch(context.Background(), inputFor(plan, c), c)
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

// TestDispatch_ReserveAddressesConcurrent stresses ReserveAddresses to confirm
// disjoint ranges under contention. This is the core invariant the parallel
// planner relies on.
func TestDispatch_ReserveAddressesConcurrent(t *testing.T) {
	c := &Context{}
	const goroutines = 16
	const perGoroutine = 1000
	const block = 10

	got := make([][]uint64, goroutines)
	done := make(chan int, goroutines)
	for g := range goroutines {
		got[g] = make([]uint64, 0, perGoroutine)
		go func(idx int) {
			for range perGoroutine {
				got[idx] = append(got[idx], c.ReserveAddresses(block))
			}
			done <- idx
		}(g)
	}
	for range goroutines {
		<-done
	}
	seen := make(map[uint64]bool, goroutines*perGoroutine)
	for _, slice := range got {
		for _, v := range slice {
			if v%block != 0 {
				t.Fatalf("start %d not multiple of block %d", v, block)
			}
			if seen[v] {
				t.Fatalf("duplicate start %d", v)
			}
			seen[v] = true
		}
	}
	wantHi := uint64(goroutines * perGoroutine * block)
	if c.LoadAddressCursor() != wantHi {
		t.Fatalf("LoadAddressCursor: want %d, got %d", wantHi, c.LoadAddressCursor())
	}
}
