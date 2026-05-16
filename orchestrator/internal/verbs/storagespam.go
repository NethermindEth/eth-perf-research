package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageSpamGasToBurn is the gasLimit argument passed to setRandomForGas.
// Spamoor's storagespam scenario defaults GasUnitsToBurn to 2_000_000; the
// orchestrator pins 1_950_000 so the contract's internal SLOAD/SSTORE loop
// stays under the verb's 2_000_000 tx gas cap.
const storageSpamGasToBurn = 1_950_000

// verbStorageSpam builds a setRandomForGas(uint256 gasLimit, uint256 txid)
// call against the deployed StorageSpam contract. Spamoor's sendTx passes
// txid = txIdx (storagespam.go: SetRandomForGas(..., big.NewInt(int64(txIdx))));
// the contract mixes txid into the storage-slot keccak, so a distinct txid per
// tx writes a distinct slot window — this is what actually bloats the storage
// trie. The orchestrator therefore threads the per-tx index into the txid
// word rather than the stub-era constant zero.
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
		Gas:   2_000_000,
	}, nil
}
