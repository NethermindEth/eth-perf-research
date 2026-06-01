package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// calltxGas is the exec-tx gas limit (EELS default execution_gas when no
// gas_limit is configured).
const calltxGas = 500_000

// verbCalltx builds a non-reverting call into the deployed StorageSpam
// contract. The orchestrator pins getStorage(uint256 key) — a single SLOAD
// that returns without reverting, warming the contract account and one storage
// slot without bloating state. The key is the per-tx index so successive calls
// touch different slots.
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
		Gas:   calltxGas,
	}, nil
}
