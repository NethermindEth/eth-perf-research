package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20BloaterGasToBurn is the gasLimit argument for setRandomForGas. erc20_-
// bloater has no Spamoor scenario; the orchestrator maps it onto the deployed
// StorageSpam contract's bulk storage-writing path (setRandomForGas loops
// SLOAD/SSTORE until a gas budget is spent), so it is a real storage bloater
// rather than the stub-era no-op call to a dead 0xdd… address.
const erc20BloaterGasToBurn = 1_950_000

// verbErc20Bloater builds a bulk storage-write call against the deployed
// StorageSpam contract. It calls setRandomForGas(uint256 gasLimit, uint256
// txid) with txid set to the per-tx index, so each tx writes a distinct window
// of storage slots — the same primary-storage-bloat mechanism as storagespam,
// kept as a separate verb so the controller can weight it independently.
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
		Gas:   2_000_000,
	}, nil
}
