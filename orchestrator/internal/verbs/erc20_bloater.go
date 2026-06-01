package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20_bloater tuning constants. The verb shares StorageSpam with the
// storagespam verb — both call setRandomForGas(uint256 gasLimit, uint256 txid)
// — but burns a far larger gas budget per tx so each call writes a wider slot
// window.
const (
	// erc20BloaterGas is the exec-tx gas limit, matching the EIP-7825 per-tx
	// cap.
	erc20BloaterGas = 16_700_000
	// erc20BloaterGasToBurn is the gasLimit argument passed to setRandomForGas:
	// exec-tx limit minus 50_000 headroom (same sizing as storageSpamExecGas).
	// Intentionally much larger than storageSpamGasToBurn so each call writes
	// more storage slots than a plain storagespam tx.
	erc20BloaterGasToBurn = erc20BloaterGas - 50_000
)

// verbErc20Bloater builds a setRandomForGas(uint256 gasLimit, uint256 txid)
// call against the deployed StorageSpam contract. The per-tx index is threaded
// into the txid word so every tx writes a distinct slot window.
type verbErc20Bloater struct{}

func (verbErc20Bloater) Name() string { return "erc20_bloater" }

func (v verbErc20Bloater) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("setRandomForGas(uint256,uint256)"),
		word32(erc20BloaterGasToBurn),
		word32(idx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   erc20BloaterGas,
	}, nil
}
