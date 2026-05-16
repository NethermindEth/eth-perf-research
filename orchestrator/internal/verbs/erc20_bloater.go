package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20BloaterAddressesPerTx is the sliding-window stride: each tx mints to
// this many sequential addresses (verbs.py _erc20_bloater_build).
const erc20BloaterAddressesPerTx = 370

// verbErc20Bloater builds a bloatStorage(uint256 startIndex, uint256 count)
// call that sweeps a sliding window of the address space.
type verbErc20Bloater struct{}

func (verbErc20Bloater) Name() string { return "erc20_bloater" }

func (verbErc20Bloater) BuildTx(idx uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrErc20Bloater
	startIndex := 1 + idx*erc20BloaterAddressesPerTx
	data := concat(
		selector("bloatStorage(uint256,uint256)"),
		word32(startIndex),
		word32(erc20BloaterAddressesPerTx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   80_000,
	}, nil
}
