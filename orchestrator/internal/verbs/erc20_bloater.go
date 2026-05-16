package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// erc20_bloater tuning constants — ported from EELS
// build_erc20_bloater_transactions (helpers.py:1168). Each exec tx calls
// bloatStorage(uint256 startSlot, uint256 numAddresses) on the deployed
// ERC20Bloater contract, sweeping the address space sequentially:
// startSlot = startAddressIndex + idx*addressesPerTx.
const (
	// erc20BloaterAddressesPerTx is the numAddresses argument (step), matching
	// helpers.py addresses_per_tx default of 370.
	erc20BloaterAddressesPerTx = 370
	// erc20BloaterStartIndex is the first startSlot, matching helpers.py
	// start_address_index default of 1.
	erc20BloaterStartIndex = 1
	// erc20BloaterGas is the exec-tx gas limit, matching helpers.py
	// _BLOAT_DEFAULT_GAS (16_700_000, the EIP-7825 per-tx cap).
	erc20BloaterGas = 16_700_000
)

// verbErc20Bloater builds a bloatStorage(uint256 startSlot, uint256
// numAddresses) call against the deployed ERC20Bloater contract. ERC20Bloater
// is the real Spamoor statebloat/erc20_bloater scenario contract; bloatStorage
// transfers tokens and approves a sequential window of addresses, creating two
// fresh storage slots per address. The orchestrator slides startSlot per tx so
// each call writes a distinct window — the same sequential sweep as the EELS
// build_erc20_bloater_transactions adaptation.
type verbErc20Bloater struct{}

func (verbErc20Bloater) Name() string { return "erc20_bloater" }

func (v verbErc20Bloater) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	start := erc20BloaterStartIndex + idx*erc20BloaterAddressesPerTx
	data := concat(
		selector("bloatStorage(uint256,uint256)"),
		word32(start),
		word32(erc20BloaterAddressesPerTx),
	)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   erc20BloaterGas,
	}, nil
}
