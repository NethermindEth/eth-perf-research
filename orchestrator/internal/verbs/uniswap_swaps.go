package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// uniswapSwapsGasToBurn is the gasLimit argument for the stand-in
// setRandomForGas call. See verbUniswapSwaps for why uniswap_swaps is mapped
// onto StorageSpam.
const uniswapSwapsGasToBurn = 1_950_000

// verbUniswapSwaps is a storage-heavy STAND-IN for the real Uniswap-V2 swap
// scenario, not a faithful port.
//
// Spamoor's uniswap-swaps scenario deploys five interdependent contracts
// (WETH9, UniswapV2Factory, UniswapV2Router02, Dai, PairLiquidityProvider),
// wires their constructors, seeds liquidity, sets per-wallet allowances, and —
// per swap — issues live eth_call queries (getAmountsIn/getAmountsOut) and
// branches on the caller's current token balance. The orchestrator's verb
// contract is a pure idx→tx function with NO RPC access during BuildTx and no
// per-wallet state, so a real swap tx (whose minOut/amountIn depend on the
// live pool reserves) cannot be constructed deterministically. Porting it
// would require an RPC-aware verb model, which is out of scope here.
//
// Rather than ship a verb that silently no-ops, uniswap_swaps is mapped onto
// the deployed StorageSpam contract's bulk storage-writing path. It produces
// real storage-trie bloat; it does NOT exercise Uniswap pair/router code. This
// is an explicit, documented stub — see the task report.
type verbUniswapSwaps struct{}

func (verbUniswapSwaps) Name() string { return "uniswap_swaps" }

func (v verbUniswapSwaps) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("setRandomForGas(uint256,uint256)"),
		word32(uniswapSwapsGasToBurn),
		word32(idx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   2_000_000,
	}, nil
}
