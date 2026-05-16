package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20txAmount is the transferMint amount (1e18 wei). Spamoor's erc20tx
// scenario defaults to 20 gwei-units; the orchestrator pins a larger fixed
// amount so the per-recipient balance word is reliably non-zero.
var erc20txAmount = func() *big.Int {
	v, _ := new(big.Int).SetString("1000000000000000000", 10)
	return v
}()

// verbErc20tx builds a transferMint(address recipient, uint256 amount) call
// against the deployed TestToken contract. TestToken.transferMint mints fresh
// tokens to recipient and writes its balance slot; Spamoor's erc20tx scenario
// picks a distinct recipient per tx (erc20tx.go: GetWallet(SelectWalletByIndex,
// txIdx+1)). The orchestrator derives a per-idx recipient the same way so each
// call writes a new balance-mapping slot — the actual ERC20 storage bloat.
type verbErc20tx struct{}

func (verbErc20tx) Name() string { return "erc20tx" }

func (v verbErc20tx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	recipient, err := deriveAddress(ctx, idx)
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("transferMint(address,uint256)"),
		addressWord(recipient),
		wordBig(erc20txAmount.Bytes()),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   100_000,
	}, nil
}
