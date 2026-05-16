package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// calltxGas is the exec-tx gas limit. EELS build_calltx_transactions
// (helpers.py:232) defaults execution_gas to 500_000 when no gas_limit is
// configured (helpers.py:342).
const calltxGas = 500_000

// verbCalltx builds a non-reverting call into the deployed StorageSpam
// contract. EELS build_calltx_transactions is a generic contract-call builder:
// the exec tx targets contract_address with caller-supplied calldata and a
// default gas of 500_000. The orchestrator pins a concrete, non-reverting call
// — getStorage(uint256 key) — which performs a single SLOAD and returns,
// warming the contract account and one storage slot without bloating state.
// The key is the per-tx index so successive calls touch different slots.
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
