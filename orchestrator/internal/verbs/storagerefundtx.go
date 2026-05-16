package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageRefundSlotsPerCall is the slotsPerCall argument passed to execute.
// Spamoor's storagerefundtx scenario defaults SlotsPerCall to 500, sizing the
// tx gas via gasLimitForSlots (~100k overhead + ~27.7k/slot). The orchestrator
// pins 40 slots so the contract's write+clear loop fits the verb's tx gas cap
// while still touching real storage every call — the stub-era port passed 0,
// which made the contract write nothing.
const storageRefundSlotsPerCall = 40

// storageRefundGas covers gasLimitForSlots(40) from the Spamoor scenario:
// 40*(22300+5200+200) + 100000 ≈ 1_208_000, rounded up for headroom.
const storageRefundGas = 1_300_000

// verbStorageRefundtx builds an execute(uint256 slotsPerCall) call against the
// deployed StorageRefund contract. StorageRefund.execute writes a window of
// fresh storage slots and clears an older window to exercise SSTORE refunds
// (storagerefundtx.go: storageRefund.Execute(slotsPerCall)).
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
