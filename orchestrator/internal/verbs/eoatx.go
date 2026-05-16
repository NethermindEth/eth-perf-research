package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// eoatxRecipient is the fixed recipient for every eoatx transfer. EELS
// build_eoatx_transactions (helpers.py:212) sends every tx to the zero
// address with value = amount (default 0); the orchestrator matches that
// exactly rather than deriving a fresh recipient per tx.
var eoatxRecipient = common.HexToAddress("0x0000000000000000000000000000000000000000")

// verbEoatx builds an EOA-to-EOA type-2 transfer. Faithful to EELS
// build_eoatx_transactions: to = 0x0, value = amount (0), data empty,
// gas 21000.
type verbEoatx struct{}

func (verbEoatx) Name() string { return "eoatx" }

func (verbEoatx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := eoatxRecipient
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  []byte{},
		Gas:   21_000,
	}, nil
}
