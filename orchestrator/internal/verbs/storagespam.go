package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageSpamGasToBurn is the gas-units argument the facade pins for
// storagespam (verbs.py _storagespam_build: gas_units_to_burn=1_950_000).
const storageSpamGasToBurn = 1_950_000

// verbStorageSpam builds a setRandomForGas(uint256 gasToBurn, uint256 seed)
// call. The facade drives the EELS builder with count=1, so the builder's
// internal loop index is always 0 and the seed word is a constant zero —
// idx-independent. (The per-tx distinctness comes from the nonce, not the
// calldata seed.)
type verbStorageSpam struct{}

func (verbStorageSpam) Name() string { return "storagespam" }

func (verbStorageSpam) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrStorageSpam
	data := concat(
		selector("setRandomForGas(uint256,uint256)"),
		word32(storageSpamGasToBurn),
		word32(0),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   2_000_000,
	}, nil
}
