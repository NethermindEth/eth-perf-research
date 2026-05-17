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

// testTipWei is the default RunConfig priority tip the fee-policy tests assert
// against (config.Defaults().Cost.PriorityTipWei — the historical 1 gwei).
var testTipWei = config.Defaults().Cost.PriorityTipWei

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
		_ = runFeePolicyLoop(ctx, f, fctx, interval, testTipWei)
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
		_ = runFeePolicyLoop(ctx, f, fctx, interval, testTipWei)
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
		_ = runFeePolicyLoop(ctx, f, fctx, 15*time.Millisecond, testTipWei)
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

func TestRefreshFeePolicyPrimesContext(t *testing.T) {
	f := &fakeFetcher{baseFee: big.NewInt(7_000_000_000), gasLimit: 12_345_678}
	fctx := &facade.Context{BaseAddress: make([]byte, 20)}

	if _, err := refreshFeePolicy(context.Background(), f, fctx, testTipWei); err != nil {
		t.Fatalf("refreshFeePolicy: %v", err)
	}

	maxFee, tip := fctx.LoadFeePolicy()
	if maxFee == nil || tip == nil {
		t.Fatal("fee policy not set")
	}
	wantTip := big.NewInt(testTipWei)
	if tip.Cmp(wantTip) != 0 {
		t.Fatalf("tip = %s, want %s", tip, wantTip)
	}
	wantMax := new(big.Int).Mul(f.baseFee, big.NewInt(2))
	wantMax.Add(wantMax, wantTip)
	if maxFee.Cmp(wantMax) != 0 {
		t.Fatalf("maxFee = %s, want %s", maxFee, wantMax)
	}
	if got := fctx.LoadBlockGasLimit(); got != f.gasLimit {
		t.Fatalf("gas limit = %d, want %d", got, f.gasLimit)
	}
}
