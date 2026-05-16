package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbCalltx builds a touch-only call to the calltx placeholder with a fixed
// 4-byte zero calldata. The facade pins call_data="0x00000000".
type verbCalltx struct{}

func (verbCalltx) Name() string { return "calltx" }

func (verbCalltx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrCalltx
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  []byte{0x00, 0x00, 0x00, 0x00},
		Gas:   40_000,
	}, nil
}
