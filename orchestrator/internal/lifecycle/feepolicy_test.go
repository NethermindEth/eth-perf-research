package lifecycle

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// testTipWei / testEthPerGas are the default RunConfig fee parameters the
// fee-policy tests assert against (config.Defaults().Cost).
var (
	testTipWei    = config.Defaults().Cost.PriorityTipWei
	testEthPerGas = config.Defaults().Cost.EthPerGasTarget
)

type fakeFetcher struct {
	calls    atomic.Uint64
	baseFee  *big.Int
	gasLimit uint64
	mu       sync.Mutex
	err      error
}

func (f *fakeFetcher) BlockByNumber(_ context.Context, _ int64) (*rpc.BlockHeader, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &rpc.BlockHeader{
		Hash:     common.Hash{},
		Number:   1,
		BaseFee:  new(big.Int).Set(f.baseFee),
		GasLimit: f.gasLimit,
	}, nil
}

func TestFeePolicyLoopOneCallPerTick(t *testing.T) {
	const interval = 25 * time.Millisecond
	const ticks = 6

	f := &fakeFetcher{baseFee: big.NewInt(1_000_000_000), gasLimit: 30_000_000}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = runFeePolicyLoop(ctx, f, fctx, interval, testEthPerGas, testTipWei)
		close(done)
	}()

	time.Sleep(time.Duration(ticks)*interval + interval/2)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fee policy loop did not exit on ctx cancel")
	}

	got := f.calls.Load()
	if got < uint64(ticks-1) || got > uint64(ticks+2) {
		t.Fatalf("call count = %d, want ~%d (interval=%v)", got, ticks, interval)
	}
}

func TestFeePolicyLoopRateIndependentOfReaders(t *testing.T) {
	const interval = 25 * time.Millisecond
	const ticks = 6
	const readers = 4

	f := &fakeFetcher{baseFee: big.NewInt(2_000_000_000), gasLimit: 30_000_000}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = runFeePolicyLoop(ctx, f, fctx, interval, testEthPerGas, testTipWei)
		close(done)
	}()

	var readerWG sync.WaitGroup
	readerWG.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer readerWG.Done()
			tick := time.NewTicker(2 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					_, _ = fctx.LoadFeePolicy()
					_ = fctx.LoadBlockGasLimit()
				}
			}
		}()
	}

	time.Sleep(time.Duration(ticks)*interval + interval/2)
	cancel()
	readerWG.Wait()
	<-done

	got := f.calls.Load()
	if got > uint64(ticks+2) {
		t.Fatalf("planners triggered extra RPC: calls=%d want <=%d (interval=%v, readers=%d)",
			got, ticks+2, interval, readers)
	}
}

func TestFeePolicyLoopSurvivesTransientError(t *testing.T) {
	f := &fakeFetcher{baseFee: big.NewInt(1_000_000_000), gasLimit: 30_000_000}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = runFeePolicyLoop(ctx, f, fctx, 15*time.Millisecond, testEthPerGas, testTipWei)
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	f.mu.Lock()
	f.err = context.DeadlineExceeded
	f.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	time.Sleep(40 * time.Millisecond)

	cancel()
	<-done

	maxFee, tip := fctx.LoadFeePolicy()
	if maxFee == nil || tip == nil {
		t.Fatal("fee policy should remain set despite transient error")
	}
	if fctx.LoadBlockGasLimit() == 0 {
		t.Fatal("block gas limit should remain set despite transient error")
	}
}

// TestRefreshFeePolicyFixedEthPerGas verifies the normal case: when base fee is
// below the configured eth-per-gas target, maxFeePerGas is the fixed target —
// not 2x base fee. The policy stays cheap and predictable.
func TestRefreshFeePolicyFixedEthPerGas(t *testing.T) {
	// Base fee 1 wei (far below the 1 gwei target) — target must dominate.
	f := &fakeFetcher{baseFee: big.NewInt(1), gasLimit: 12_345_678}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	if err := refreshFeePolicy(context.Background(), f, fctx, testEthPerGas, testTipWei); err != nil {
		t.Fatalf("refreshFeePolicy: %v", err)
	}

	maxFee, tip := fctx.LoadFeePolicy()
	if maxFee == nil || tip == nil {
		t.Fatal("fee policy not set")
	}
	if tip.Cmp(big.NewInt(testTipWei)) != 0 {
		t.Fatalf("tip = %s, want %d", tip, testTipWei)
	}
	if maxFee.Cmp(big.NewInt(testEthPerGas)) != 0 {
		t.Fatalf("maxFee = %s, want fixed target %d", maxFee, testEthPerGas)
	}
	if got := fctx.LoadBlockGasLimit(); got != f.gasLimit {
		t.Fatalf("gas limit = %d, want %d", got, f.gasLimit)
	}
}

// TestRefreshFeePolicyFloorsOnBaseFeeSpike verifies that when base fee spikes
// above the eth-per-gas target, maxFeePerGas falls back to the baseFee+tip
// floor so a tx can never be rejected for underpricing.
func TestRefreshFeePolicyFloorsOnBaseFeeSpike(t *testing.T) {
	// Base fee 7 gwei — far above the 1 gwei target.
	spike := big.NewInt(7_000_000_000)
	f := &fakeFetcher{baseFee: spike, gasLimit: 12_345_678}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	if err := refreshFeePolicy(context.Background(), f, fctx, testEthPerGas, testTipWei); err != nil {
		t.Fatalf("refreshFeePolicy: %v", err)
	}

	maxFee, _ := fctx.LoadFeePolicy()
	wantFloor := new(big.Int).Add(spike, big.NewInt(testTipWei))
	if maxFee.Cmp(wantFloor) != 0 {
		t.Fatalf("maxFee = %s, want baseFee+tip floor %s", maxFee, wantFloor)
	}
}
