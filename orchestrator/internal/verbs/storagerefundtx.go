package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageRefundSlotsPerCall is the slotsPerCall argument passed to execute
// (EELS default: 500).
const storageRefundSlotsPerCall = 500

// storageRefundGas is the exec-tx gas limit, sized to fit SlotsPerCall<=500
// SSTORE+clear cycles under a 30M block cap.
const storageRefundGas = 3_000_000

// verbStorageRefundtx builds an execute(uint256 slotsPerCall) call against the
// deployed StorageRefund contract. StorageRefund.execute writes a window of
// fresh storage slots and clears an older window to exercise SSTORE refunds.
type verbStorageRefundtx struct{}

func (verbStorageRefundtx) Name() string { return "storagerefundtx" }

func (v verbStorageRefundtx) BuildTx(_ uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("execute(uint256)"),
		word32(storageRefundSlotsPerCall),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   storageRefundGas,
	}, nil
}
