package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbStorageRefundtx builds an execute(uint256 slots) call. The facade pins
// slots_per_call=0 so the SSTORE-to-zero refund marker survives in calldata —
// the argument is a 32-byte zero word, idx-independent.
type verbStorageRefundtx struct{}

func (verbStorageRefundtx) Name() string { return "storagerefundtx" }

func (verbStorageRefundtx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrStorageRefund
	data := concat(
		selector("execute(uint256)"),
		word32(0),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   80_000,
	}, nil
}
