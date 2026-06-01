package verbs

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// erc20txAmount is the transferMint amount (1e18 wei), pinned so the
// per-recipient balance word is non-zero.
var erc20txAmount = func() *big.Int {
	v, _ := new(big.Int).SetString("1000000000000000000", 10)
	return v
}()

// verbErc20tx builds a transferMint(address recipient, uint256 amount) call
// against the deployed TestToken contract. The recipient is derived per-idx:
// a 20-byte address with a fixed 0xcc-repeated prefix and the low 8 bytes set
// to the tx index, guaranteeing a unique balance slot per tx.
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

// erc20RecipientForIdx returns a 20-byte address: 0xcc repeated 12 times
// followed by the low 8 bytes of idx big-endian.
func erc20RecipientForIdx(idx uint64) common.Address {
	var addr common.Address
	for i := 0; i < 12; i++ {
		addr[i] = 0xcc
	}
	binary.BigEndian.PutUint64(addr[12:], idx)
	return addr
}
