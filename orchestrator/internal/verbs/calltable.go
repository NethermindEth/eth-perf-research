package verbs

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// callSpec is a table-driven specification for the family of contract-call
// verbs whose template is fully described by a fixed shape: resolve the verb's
// target contract via BuildCtx.Contracts, build a per-tx calldata payload, set
// Value to zero, and use a constant gas limit. Verbs whose template needs any
// branching beyond this (per-idx target/amount derivation, value transfers,
// multiple call variants) are kept bespoke and are NOT expressed here.
type callSpec struct {
	// name is the verb's registry key. It also keys the target-contract lookup
	// in contractVerbTargets, so it must match an entry there.
	name string
	// gas is the constant exec-tx gas limit for every tx this verb builds.
	gas uint64
	// data builds the calldata payload for tx idx. It is the only per-tx
	// degree of freedom in this verb family.
	data func(idx uint64) []byte
}

// callVerb is the single generic Verb implementation over a callSpec. Every
// folded contract-call verb is an instance of this type — there are no
// per-verb method bodies left to drift.
type callVerb struct {
	spec callSpec
}

func (v callVerb) Name() string { return v.spec.name }

func (v callVerb) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.spec.name)
	if err != nil {
		return nil, err
	}
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  v.spec.data(idx),
		Gas:   v.spec.gas,
	}, nil
}

// --- per-verb tuning constants -------------------------------------------

// calltxGas is the exec-tx gas limit for the calltx verb (EELS default
// execution_gas when no gas_limit is configured).
const calltxGas = 500_000

// storageSpamGasToBurn is the gasLimit argument passed to setRandomForGas by
// the storagespam verb (EELS default gas_units_to_burn). Each exec tx is sized
// at gas_units_to_burn + 50_000.
const storageSpamGasToBurn = 2_000_000

// storageSpamExecGas is the storagespam exec-tx gas limit:
// gas_units_to_burn + 50_000.
const storageSpamExecGas = storageSpamGasToBurn + 50_000

// erc20_bloater tuning constants. The verb shares the StorageSpam contract with
// the storagespam verb — both call setRandomForGas(uint256 gasLimit,
// uint256 txid) — but burns a far larger gas budget per tx so each call writes
// a wider slot window.
const (
	// erc20BloaterGas is the erc20_bloater exec-tx gas limit, matching the
	// EIP-7825 per-tx cap.
	erc20BloaterGas = 16_700_000
	// erc20BloaterGasToBurn is the gasLimit argument passed to setRandomForGas:
	// exec-tx limit minus 50_000 headroom (same sizing as storageSpamExecGas).
	// Intentionally much larger than storageSpamGasToBurn so each call writes
	// more storage slots than a plain storagespam tx.
	erc20BloaterGasToBurn = erc20BloaterGas - 50_000
)

// storageRefundSlotsPerCall is the slotsPerCall argument passed to execute by
// the storagerefundtx verb (EELS default: 500).
const storageRefundSlotsPerCall = 500

// storageRefundGas is the storagerefundtx exec-tx gas limit, sized to fit
// SlotsPerCall<=500 SSTORE+clear cycles under a 30M block cap.
const storageRefundGas = 3_000_000

// gasburnerGasUnitsToBurn is the gasburnertx exec-tx gas limit (Spamoor/EELS
// default GasUnitsToBurn).
const gasburnerGasUnitsToBurn = 2_000_000

// --- call-verb specs ------------------------------------------------------

// callSpecs holds the specifications for every contract-call verb folded into
// the generic callVerb. Adding a trivially-shaped call verb is a one-line
// table entry here plus its contractVerbTargets/verbContractDeps registration.
//
//   - calltx          getStorage(uint256 key) — a single non-reverting SLOAD
//     that warms the contract account and one slot; key is the per-tx index so
//     successive calls touch different slots.
//   - storagespam     setRandomForGas(uint256 gasLimit, uint256 txid) — the
//     per-tx index is threaded into txid so every tx writes a distinct slot
//     window, the primary storage-trie bloater.
//   - erc20_bloater   same setRandomForGas call as storagespam but with a much
//     larger gas budget so each call writes a wider slot window.
//   - storagerefundtx execute(uint256 slotsPerCall) — writes a window of fresh
//     slots and clears an older window to exercise SSTORE refunds; ignores idx.
//   - gasburnertx     raw 4-byte big-endian tx index (no selector); the
//     GasBurner runtime ignores calldata and loops burning gas, then emits one
//     LOG1.
var callSpecs = []callSpec{
	{
		name: "calltx",
		gas:  calltxGas,
		data: func(idx uint64) []byte {
			return concat(
				selector("getStorage(uint256)"),
				word32(idx),
			)
		},
	},
	{
		name: "storagespam",
		gas:  storageSpamExecGas,
		data: func(idx uint64) []byte {
			return concat(
				selector("setRandomForGas(uint256,uint256)"),
				word32(storageSpamGasToBurn),
				word32(idx),
			)
		},
	},
	{
		name: "erc20_bloater",
		gas:  erc20BloaterGas,
		data: func(idx uint64) []byte {
			return concat(
				selector("setRandomForGas(uint256,uint256)"),
				word32(erc20BloaterGasToBurn),
				word32(idx),
			)
		},
	},
	{
		name: "storagerefundtx",
		gas:  storageRefundGas,
		data: func(_ uint64) []byte {
			return concat(
				selector("execute(uint256)"),
				word32(storageRefundSlotsPerCall),
			)
		},
	},
	{
		name: "gasburnertx",
		gas:  gasburnerGasUnitsToBurn,
		data: func(idx uint64) []byte {
			payload := make([]byte, 4)
			binary.BigEndian.PutUint32(payload, uint32(idx))
			return payload
		},
	},
}
