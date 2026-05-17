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
// and block gas limit into the facade context. ethPerGasTarget and
// priorityTipWei are the resolved RunConfig fixed fee parameters. Returns the
// fetched header so startup callers can use it for one-shot priming before
// launching the loop.
//
// The fee policy is a fixed ETH-per-gas target — bloating stays cheap and
// predictable rather than tracking 2x base fee. maxFeePerGas is the configured
// target, floored at baseFee+tip so a base-fee spike above target can never
// cause an underpriced-tx rejection. This is pure cost control; it is invisible
// to verb selection.
func refreshFeePolicy(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, ethPerGasTarget, priorityTipWei int64) (*rpc.BlockHeader, error) {
	head, err := fetcher.BlockByNumber(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: head for fee policy: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	tip := big.NewInt(priorityTipWei)
	maxFee := big.NewInt(ethPerGasTarget)
	floor := new(big.Int).Add(baseFee, tip)
	if floor.Cmp(maxFee) > 0 {
		maxFee = floor
	}
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
func runFeePolicyLoop(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, interval time.Duration, ethPerGasTarget, priorityTipWei int64) error {
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
			if _, err := refreshFeePolicy(ctx, fetcher, fctx, ethPerGasTarget, priorityTipWei); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn("lifecycle: fee policy refresh failed", "err", err)
			}
		}
	}
}
