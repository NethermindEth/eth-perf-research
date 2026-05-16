package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// uniswapSwapCalldata is the swapExactTokensForETH calldata the facade emits.
// The facade drives the EELS builder with count=1: with buy_ratio defaulting
// such that buy_count_target=0 for a single tx, every tx takes the sell
// branch (variant 2 = swapExactTokensForETH, path=[token, weth]). The swap
// amount is the fixed midpoint of [min, max] and the deadline is the pinned
// constant 2_000_000_000, so the calldata is fully idx-independent.
//
// Ported verbatim (byte-for-byte) from the Python oracle rather than
// re-deriving the dynamic-array ABI encoding, since the value is constant.
//
// Layout: selector 0x18cbafe5
//   amountIn  | minOut | path offset (0xa0) | recipient | deadline
//   path length (2) | token (0x6666..) | weth (0x5555..)
var uniswapSwapCalldata = mustHex(
	"18cbafe5" +
		"00000000000000000000000000000000000000000000001b1b96799f1e150000" +
		"00000000000000000000000000000000000000000000001af8e3cd7e52696000" +
		"00000000000000000000000000000000000000000000000000000000000000a0" +
		"0000000000000000000000007777777777777777777777777777777777777777" +
		"0000000000000000000000000000000000000000000000000000000077359400" +
		"0000000000000000000000000000000000000000000000000000000000000002" +
		"0000000000000000000000006666666666666666666666666666666666666666" +
		"0000000000000000000000005555555555555555555555555555555555555555",
)

// verbUniswapSwaps builds a Uniswap-V2 router swap call. See uniswapSwapCalldata
// for why the calldata is constant.
type verbUniswapSwaps struct{}

func (verbUniswapSwaps) Name() string { return "uniswap_swaps" }

func (verbUniswapSwaps) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrUniswapSwaps
	data := append([]byte(nil), uniswapSwapCalldata...)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   250_000,
	}, nil
}
