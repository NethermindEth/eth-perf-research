package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20_bloater tuning constants. The verb has no canonical Spamoor scenario
// contract: it shares StorageSpam with the storagespam verb. Both verbs bloat
// the storage trie via setRandomForGas(uint256 gasLimit, uint256 txid); the
// only difference is erc20_bloater burns a far larger gas budget per tx, so
// each call writes a wider slot window — matching the "bloater" intent of
// bulk state growth.
const (
	// erc20BloaterGas is the exec-tx gas limit, matching the EIP-7825 per-tx
	// cap used by the EELS _BLOAT_DEFAULT_GAS.
	erc20BloaterGas = 16_700_000
	// erc20BloaterGasToBurn is the gasLimit argument passed to
	// setRandomForGas: the exec-tx limit minus 50_000 headroom, mirroring the
	// storagespam verb's gas_units + 50_000 sizing. It is intentionally much
	// larger than storageSpamGasToBurn so each erc20_bloater tx writes more
	// storage slots than a plain storagespam tx.
	erc20BloaterGasToBurn = erc20BloaterGas - 50_000
)

// verbErc20Bloater builds a setRandomForGas(uint256 gasLimit, uint256 txid)
// call against the deployed StorageSpam contract. erc20_bloater has no
// dedicated Spamoor scenario contract; it reuses StorageSpam — the real
// storage bloater — and simply burns a larger gas budget per tx so each call
// grows more of the storage trie than the storagespam verb. The per-tx index
// is threaded into the txid word so every tx writes a distinct slot window.
type verbErc20Bloater struct{}

func (verbErc20Bloater) Name() string { return "erc20_bloater" }

func (v verbErc20Bloater) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	data := concat(
		selector("setRandomForGas(uint256,uint256)"),
		word32(erc20BloaterGasToBurn),
		word32(idx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   erc20BloaterGas,
	}, nil
}
