package verbs

import (
	"bytes"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbBlobCombined is a type-2 stub that mirrors the Python facade stub
// (verbs.py _blob_combined_build). Real EELS blob_combined emits type-3 txs
// requiring KZG sidecars, which the signer does not yet produce — deferred to
// a follow-up. The stub payload is a fixed 32-byte 0xff blob.
type verbBlobCombined struct{}

func (verbBlobCombined) Name() string { return "blob_combined" }

func (verbBlobCombined) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrBlobCombined
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  bytes.Repeat([]byte{0xff}, 32),
		Gas:   200_000,
	}, nil
}
