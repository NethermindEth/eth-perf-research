package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

const defaultFeePolicyInterval = time.Second

// defaultTargetEthPerGasWei is the fixed gas price (1 gwei) the orchestrator
// pins the effective EIP-1559 fee to when ORCH_TARGET_ETH_PER_GAS is unset.
const defaultTargetEthPerGasWei = 1_000_000_000

// feePolicyUnreachableWarnOnce gates the WARN logged when the live baseFee
// exceeds the fixed target so the loop does not spam the log every tick.
var feePolicyUnreachableWarnOnce sync.Once

// headBlockFetcher is the minimal RPC surface the fee-policy loop needs.
// The lifecycle passes a *rpc.Client; tests substitute an in-memory fake.
type headBlockFetcher interface {
	BlockByNumber(ctx context.Context, n int64) (*rpc.BlockHeader, error)
}

// refreshFeePolicy fetches the latest head and writes the EIP-1559 fee policy
// and block gas limit into the facade context. Returns the fetched header so
// startup callers can use it for one-shot priming before launching the loop.
//
// The effective gas price (baseFee + priorityFee) is pinned to
// targetEthPerGasWei: maxPriorityFeePerGas = max(0, target - baseFee) and
// maxFeePerGas = target. When baseFee exceeds the target the pin would
// underprice the tx, so we fall back to baseFee + 1 gwei (with a 1 gwei tip)
// and warn once.
func refreshFeePolicy(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, targetEthPerGasWei uint64) (*rpc.BlockHeader, error) {
	head, err := fetcher.BlockByNumber(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: head for fee policy: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}

	target := new(big.Int).SetUint64(targetEthPerGasWei)
	var maxFee, tip *big.Int
	if baseFee.Cmp(target) <= 0 {
		// Target reachable: pin effective price to it exactly.
		tip = new(big.Int).Sub(target, baseFee)
		if tip.Sign() < 0 {
			tip = new(big.Int)
		}
		maxFee = new(big.Int).Set(target)
	} else {
		// baseFee above the target — pinning would underprice the tx. Fall back
		// to a 1 gwei tip over the live baseFee so the tx still lands.
		tip = big.NewInt(defaultPriorityTipWei)
		maxFee = new(big.Int).Add(baseFee, tip)
		feePolicyUnreachableWarnOnce.Do(func() {
			slog.Warn("lifecycle: baseFee exceeds ORCH_TARGET_ETH_PER_GAS, falling back to baseFee + 1 gwei",
				"base_fee_wei", baseFee.String(),
				"target_eth_per_gas_wei", target.String())
		})
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
func runFeePolicyLoop(ctx context.Context, fetcher headBlockFetcher, fctx *facade.Context, interval time.Duration, targetEthPerGasWei uint64) error {
	if interval <= 0 {
		interval = defaultFeePolicyInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if _, err := refreshFeePolicy(ctx, fetcher, fctx, targetEthPerGasWei); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn("lifecycle: fee policy refresh failed", "err", err)
			}
		}
	}
}
