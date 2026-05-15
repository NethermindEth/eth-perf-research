package lifecycle

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
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
		_ = runFeePolicyLoop(ctx, f, fctx, interval, defaultTargetEthPerGasWei)
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
		_ = runFeePolicyLoop(ctx, f, fctx, interval, defaultTargetEthPerGasWei)
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
		_ = runFeePolicyLoop(ctx, f, fctx, 15*time.Millisecond, defaultTargetEthPerGasWei)
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

// TestRefreshFeePolicyPinsToTarget: when baseFee is below the fixed target the
// effective gas price (baseFee + tip) is pinned exactly to the target —
// maxFeePerGas = target, maxPriorityFeePerGas = target - baseFee.
func TestRefreshFeePolicyPinsToTarget(t *testing.T) {
	const target uint64 = 5_000_000_000 // 5 gwei
	baseFee := big.NewInt(2_000_000_000) // 2 gwei, below target
	f := &fakeFetcher{baseFee: baseFee, gasLimit: 12_345_678}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	if _, err := refreshFeePolicy(context.Background(), f, fctx, target); err != nil {
		t.Fatalf("refreshFeePolicy: %v", err)
	}

	maxFee, tip := fctx.LoadFeePolicy()
	if maxFee == nil || tip == nil {
		t.Fatal("fee policy not set")
	}
	wantTip := new(big.Int).Sub(new(big.Int).SetUint64(target), baseFee)
	if tip.Cmp(wantTip) != 0 {
		t.Fatalf("tip = %s, want %s (target - baseFee)", tip, wantTip)
	}
	wantMax := new(big.Int).SetUint64(target)
	if maxFee.Cmp(wantMax) != 0 {
		t.Fatalf("maxFee = %s, want %s (pinned to target)", maxFee, wantMax)
	}
	// Effective price baseFee + tip must equal the target exactly.
	effective := new(big.Int).Add(baseFee, tip)
	if effective.Cmp(wantMax) != 0 {
		t.Fatalf("effective price = %s, want %s", effective, wantMax)
	}
	if got := fctx.LoadBlockGasLimit(); got != f.gasLimit {
		t.Fatalf("gas limit = %d, want %d", got, f.gasLimit)
	}
}

// TestRefreshFeePolicyBaseFeeAboveTargetFallsBack: when baseFee exceeds the
// fixed target, pinning would underprice the tx, so the policy falls back to
// baseFee + 1 gwei with a 1 gwei tip.
func TestRefreshFeePolicyBaseFeeAboveTargetFallsBack(t *testing.T) {
	const target uint64 = 1_000_000_000 // 1 gwei
	baseFee := big.NewInt(7_000_000_000) // 7 gwei, above target
	f := &fakeFetcher{baseFee: baseFee, gasLimit: 12_345_678}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	if _, err := refreshFeePolicy(context.Background(), f, fctx, target); err != nil {
		t.Fatalf("refreshFeePolicy: %v", err)
	}

	maxFee, tip := fctx.LoadFeePolicy()
	if maxFee == nil || tip == nil {
		t.Fatal("fee policy not set")
	}
	wantTip := big.NewInt(defaultPriorityTipWei)
	if tip.Cmp(wantTip) != 0 {
		t.Fatalf("tip = %s, want %s (1 gwei fallback)", tip, wantTip)
	}
	wantMax := new(big.Int).Add(baseFee, wantTip)
	if maxFee.Cmp(wantMax) != 0 {
		t.Fatalf("maxFee = %s, want %s (baseFee + 1 gwei)", maxFee, wantMax)
	}
}
