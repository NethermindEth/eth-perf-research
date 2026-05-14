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
	// (Dispatch no longer treats this as authoritative — kept for the
	// fake-builder contract.)
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
		BaseAddress:    make([]byte, 20),
		Revision:       1,
		AddressStride:  1 << 40,
		ChainID:        1,
		GasLimit:       30_000_000,
		VerbGasFactors: map[string]float64{},
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
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: 42}
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
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: 99}
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
// caller-supplied StartSalt + NumNonces. Dispatch no longer reads the
// builder's response NewSaltCursor as authoritative.
func TestDispatch_SaltCursorAdvance(t *testing.T) {
	const builderEcho = uint64(777) // ignored by Dispatch
	fb := &fakeBuilder{txGas: 21_000, newSaltCursor: builderEcho}
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

// TestDispatch_WorkerError verifies that a non-empty BuildBatchResponse.Error
// is surfaced as a Go error.
func TestDispatch_WorkerError(t *testing.T) {
	fb := &fakeBuilder{errMsg: "worker exploded"}
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
	for g := 0; g < goroutines; g++ {
		got[g] = make([]uint64, 0, perGoroutine)
		go func(idx int) {
			for i := 0; i < perGoroutine; i++ {
				got[idx] = append(got[idx], c.ReserveAddresses(block))
			}
			done <- idx
		}(g)
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	// All starts must be unique multiples of block and cover 0..goroutines*perGoroutine*block
	// (modulo ordering across goroutines).
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
