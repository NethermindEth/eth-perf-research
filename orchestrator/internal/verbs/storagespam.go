package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageSpamGasToBurn is the gasLimit argument passed to setRandomForGas
// (EELS default gas_units_to_burn). Each exec tx is sized at
// gas_units_to_burn + 50_000.
const storageSpamGasToBurn = 2_000_000

// storageSpamExecGas is the exec-tx gas limit: gas_units_to_burn + 50_000.
const storageSpamExecGas = storageSpamGasToBurn + 50_000

// verbStorageSpam builds a setRandomForGas(uint256 gasLimit, uint256 txid)
// call against the deployed StorageSpam contract. The contract mixes txid into
// the storage-slot keccak, so a distinct txid per tx writes a distinct slot
// window — threading the per-tx index into the txid word is what actually
// bloats the storage trie.
type verbStorageSpam struct{}

func (verbStorageSpam) Name() string { return "storagespam" }

func (v verbStorageSpam) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("setRandomForGas(uint256,uint256)"),
		word32(storageSpamGasToBurn),
		word32(idx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   storageSpamExecGas,
	}, nil
}
