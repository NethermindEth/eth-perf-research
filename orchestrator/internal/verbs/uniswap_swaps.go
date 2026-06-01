package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Uniswap V2 Router02 swap selectors and fixed placeholder addresses. The
// router/WETH/DAI/recipient addresses are the EELS placeholders; targeting the
// same router placeholder keeps the produced swap calldata byte-equal to the
// EELS adaptation.
var (
	uniswapRouterAddr = common.HexToAddress("0x4444444444444444444444444444444444444444")
	uniswapWethAddr   = common.HexToAddress("0x5555555555555555555555555555555555555555")
	uniswapDaiAddr    = common.HexToAddress("0x6666666666666666666666666666666666666666")
	uniswapRecipient  = common.HexToAddress("0x7777777777777777777777777777777777777777")
)

// uniswap swap parameters.
const (
	uniswapBuyRatio = 40      // percent of txs that are buys
	uniswapSlippage = 50      // bps slippage
	uniswapGas      = 200_000 // exec-tx gas limit
)

// uniswap swap-amount bounds. swap_amount is the midpoint of [min, max];
// min_out applies the slippage haircut.
var (
	uniswapMinSwapAmount = bigFromDec("100000000000000000")            // 1e17
	uniswapMaxSwapAmount = bigFromDec("1000000000000000000000")        // 1e21
	uniswapSwapAmount    = new(big.Int).Rsh(uniswapSwapAmountSum(), 1) // (min+max)/2
	uniswapMinOut        = uniswapComputeMinOut()
	uniswapDeadline      = big.NewInt(2_000_000_000)
)

func uniswapSwapAmountSum() *big.Int {
	return new(big.Int).Add(uniswapMinSwapAmount, uniswapMaxSwapAmount)
}

// uniswapComputeMinOut: min_out = swap_amount * (10000 - slippage) / 10000.
func uniswapComputeMinOut() *big.Int {
	num := new(big.Int).Mul(uniswapSwapAmount, big.NewInt(10_000-uniswapSlippage))
	return num.Div(num, big.NewInt(10_000))
}

func bigFromDec(s string) *big.Int {
	v, _ := new(big.Int).SetString(s, 10)
	return v
}

// verbUniswapSwaps builds Uniswap V2 router swap calls. Each tx is a type-2
// call to the router placeholder carrying ABI-encoded swap calldata.
//
// The per-idx buy/sell decision is expressed count-independently: idx is a
// buy when floor(idx*buy_ratio/100) increments at idx. Buys alternate variant 0
// (swapExactTokensForTokens) and variant 1 (swapExactETHForTokens) by buy
// parity; sells use variant 2 (swapExactTokensForETH).
type verbUniswapSwaps struct{}

func (verbUniswapSwaps) Name() string { return "uniswap_swaps" }

func (verbUniswapSwaps) BuildTx(idx uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	buysBefore := idx * uniswapBuyRatio / 100
	buysUpto := (idx + 1) * uniswapBuyRatio / 100
	isBuy := buysUpto > buysBefore

	var variant int
	var path []common.Address
	if isBuy {
		// buysBefore is the index of this buy in the buy sequence.
		if buysBefore%2 == 0 {
			variant = 0
		} else {
			variant = 1
		}
		path = []common.Address{uniswapWethAddr, uniswapDaiAddr}
	} else {
		variant = 2
		path = []common.Address{uniswapDaiAddr, uniswapWethAddr}
	}

	data, value := encodeUniswapSwapCall(variant, path)
	to := uniswapRouterAddr
	return &types.DynamicFeeTx{
		To:    &to,
		Value: value,
		Data:  data,
		Gas:   uniswapGas,
	}, nil
}

// encodeUniswapSwapCall returns the ABI-encoded calldata and tx value for a
// router swap:
//
//	variant 0 -> swapExactTokensForTokens (value 0)
//	variant 1 -> swapExactETHForTokens   (value = amountIn, payable)
//	variant 2 -> swapExactTokensForETH   (value 0)
func encodeUniswapSwapCall(variant int, path []common.Address) ([]byte, *big.Int) {
	if variant == 1 {
		// swapExactETHForTokens(uint256 amountOutMin, address[] path,
		//   address to, uint256 deadline) — amountIn == msg.value.
		data := concat(
			selector("swapExactETHForTokens(uint256,address[],address,uint256)"),
			wordBig(uniswapMinOut.Bytes()),
			word32(0x80), // offset to path array
			addressWord(uniswapRecipient),
			wordBig(uniswapDeadline.Bytes()),
			encodeAddressArray(path),
		)
		return data, new(big.Int).Set(uniswapSwapAmount)
	}

	var sel []byte
	if variant == 2 {
		sel = selector("swapExactTokensForETH(uint256,uint256,address[],address,uint256)")
	} else {
		sel = selector("swapExactTokensForTokens(uint256,uint256,address[],address,uint256)")
	}
	data := concat(
		sel,
		wordBig(uniswapSwapAmount.Bytes()),
		wordBig(uniswapMinOut.Bytes()),
		word32(0xa0), // offset to path array (5 head words precede it)
		addressWord(uniswapRecipient),
		wordBig(uniswapDeadline.Bytes()),
		encodeAddressArray(path),
	)
	return data, new(big.Int)
}

// encodeAddressArray ABI-encodes a dynamic address[] tail: length word
// followed by one left-padded word per address.
func encodeAddressArray(addrs []common.Address) []byte {
	out := word32(uint64(len(addrs)))
	for _, a := range addrs {
		out = concat(out, addressWord(a))
	}
	return out
}
