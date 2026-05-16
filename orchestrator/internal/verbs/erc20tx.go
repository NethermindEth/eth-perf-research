package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// erc20txAmount is the transferMint amount the facade pins (1e18 wei).
var erc20txAmount = func() *big.Int {
	v, _ := new(big.Int).SetString("1000000000000000000", 10)
	return v
}()

// verbErc20tx builds a transferMint(address recipient, uint256 amount) call.
// The facade drives the EELS builder with count=1, so the recipient is always
// _erc20_recipient_for_idx(0) = 0xcc*12 || 16 zero bytes — idx-independent.
type verbErc20tx struct{}

func (verbErc20tx) Name() string { return "erc20tx" }

func (verbErc20tx) BuildTx(_ uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrErc20tx
	recipient := common.HexToAddress("0xcccccccccccccccccccccccc0000000000000000")
	data := concat(
		selector("transferMint(address,uint256)"),
		addressWord(recipient),
		wordBig(erc20txAmount.Bytes()),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   60_000,
	}, nil
}
