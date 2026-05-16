package verbs

import (
	"encoding/hex"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// deploytxDefaultBytecode is the init code every deploytx creation tx carries.
// EELS build_deploytx_transactions (helpers.py:1406) cycles a configured list
// of bytecodes; with no bytecodes configured _parse_deploytx_bytecodes
// (helpers.py:1402) falls back to the single default _DEPLOYTX_DEFAULT_BYTECODE
// = 0x6001600055 (PUSH1 0x01 / PUSH1 0x00 / SSTORE — writes one slot on
// deploy, leaving an empty runtime). The orchestrator runs the no-config path,
// so every tx deploys this same init code.
var deploytxDefaultBytecode = mustHex("6001600055")

// deploytxGas is the creation-tx gas limit, matching EELS
// build_deploytx_transactions (helpers.py:1447): exec_gas defaults to
// 1_000_000 when no gas_limit is configured.
const deploytxGas = 1_000_000

// verbDeploytx builds a contract-creation tx. Faithful to the EELS
// build_deploytx_transactions adaptation with an empty bytecodes config: to =
// "" (creation), data = 0x6001600055, gas = 1_000_000.
type verbDeploytx struct{}

func (verbDeploytx) Name() string { return "deploytx" }

func (verbDeploytx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	return &types.DynamicFeeTx{
		To:    nil, // contract creation
		Value: new(big.Int),
		Data:  deploytxDefaultBytecode,
		Gas:   deploytxGas,
	}, nil
}

// mustHex decodes a compile-time hex constant or panics — used only for
// statically-known byte arrays ported from the EELS builders.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(errors.New("verbs: invalid hex constant: " + s))
	}
	return b
}
