package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbGasburnertx builds a gas-burner exec call. The facade drives the EELS
// builder with count=1 and keeps txs[1] (the exec tx), whose calldata is the
// 4-byte big-endian tx index 0 — idx-independent.
type verbGasburnertx struct{}

func (verbGasburnertx) Name() string { return "gasburnertx" }

func (verbGasburnertx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrGasburnertx
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  []byte{0x00, 0x00, 0x00, 0x00},
		Gas:   1_500_000,
	}, nil
}
