package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbCalltx builds a cheap, non-reverting call into the deployed StorageSpam
// contract. The design intent is "touch existing state, low yield": it invokes
// the view method getStorage(uint256 key), which performs a single SLOAD and
// returns. This warms the contract account and one storage slot without
// bloating state — and, critically, never reverts (the stub-era 0x00000000
// calldata hit a no-selector fallback on a dead address). The key is the
// per-tx index so successive calls touch different slots.
type verbCalltx struct{}

func (verbCalltx) Name() string { return "calltx" }

func (v verbCalltx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("getStorage(uint256)"),
		word32(idx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   40_000,
	}, nil
}
