package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// headBlockFetcher is the minimal RPC surface the fee-policy loop needs.
// The lifecycle passes a *rpc.Client; tests substitute an in-memory fake.
type headBlockFetcher interface {
	BlockByNumber(ctx context.Context, n int64) (*rpc.BlockHeader, error)
}

// refreshFeePolicy fetches the latest head and writes the EIP-1559 fee policy
// and block gas limit into the facade context. priorityTipWei is the resolved
// RunConfig fixed priority tip. Returns the fetched header so startup callers
// can use it for one-shot priming before launching the loop.
func refreshFeePolicy(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, priorityTipWei int64) (*rpc.BlockHeader, error) {
	head, err := fetcher.BlockByNumber(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: head for fee policy: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	tip := big.NewInt(priorityTipWei)
	maxFee := new(big.Int).Mul(baseFee, big.NewInt(2))
	maxFee.Add(maxFee, tip)
	fctx.SetFeePolicy(maxFee, tip)
	if head.GasLimit > 0 {
		fctx.SetBlockGasLimit(head.GasLimit)
	}
	return head, nil
}

// runFeePolicyLoop ticks at interval and refreshes the facade fee policy from
// the latest block. Planners read the policy via fctx.LoadFeePolicy() and
// fctx.LoadBlockGasLimit() without contacting the RPC themselves, so the
// orchestrator emits exactly one eth_getBlockByNumber per tick regardless of
// planner count.
//
// Exits when ctx is cancelled. RPC errors are logged at warn level and do not
// terminate the loop — the previous policy stays in place until the next tick
// succeeds.
func runFeePolicyLoop(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, interval time.Duration, priorityTipWei int64) error {
	if interval <= 0 {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if _, err := refreshFeePolicy(ctx, fetcher, fctx, priorityTipWei); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn("lifecycle: fee policy refresh failed", "err", err)
			}
		}
	}
}
