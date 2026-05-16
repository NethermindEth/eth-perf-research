package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbNoop builds a zero-value self-transfer to the signer's own address.
// Used by the controller's residual-stop logic; has no EELS analog.
type verbNoop struct{}

func (verbNoop) Name() string { return "noop" }

func (verbNoop) BuildTx(_ uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to := ctx.SignerAddr
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  []byte{},
		Gas:   21_000,
	}, nil
}
