package verbs

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// erc20txAmount is the transferMint amount. EELS build_erc20tx_transactions
// (helpers.py:941) defaults amount to 1e18 wei (random_amount=False), which
// the orchestrator pins so the per-recipient balance word is non-zero.
var erc20txAmount = func() *big.Int {
	v, _ := new(big.Int).SetString("1000000000000000000", 10)
	return v
}()

// verbErc20tx builds a transferMint(address recipient, uint256 amount) call
// against the deployed TestToken contract. TestToken.transferMint mints fresh
// tokens to recipient and writes its balance slot. The recipient is derived
// per-idx with the exact EELS formula (_erc20_recipient_for_idx,
// helpers.py:936): a 20-byte address with a fixed 0xcc-repeated prefix and the
// low 8 bytes set to the tx index. random_target is False so this deterministic
// form is used.
type verbErc20tx struct{}

func (verbErc20tx) Name() string { return "erc20tx" }

func (v verbErc20tx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("transferMint(address,uint256)"),
		addressWord(erc20RecipientForIdx(idx)),
		wordBig(erc20txAmount.Bytes()),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   100_000,
	}, nil
}

// erc20RecipientForIdx reproduces EELS _erc20_recipient_for_idx
// (helpers.py:936): tail = f"{idx:040x}"; recipient = 0x + "cc"*12 +
// tail[-16:]. The 20-byte address is therefore the byte 0xcc repeated 12
// times followed by the low 8 bytes (16 hex digits) of idx, big-endian.
func erc20RecipientForIdx(idx uint64) common.Address {
	var addr common.Address
	for i := 0; i < 12; i++ {
		addr[i] = 0xcc
	}
	binary.BigEndian.PutUint64(addr[12:], idx)
	return addr
}
