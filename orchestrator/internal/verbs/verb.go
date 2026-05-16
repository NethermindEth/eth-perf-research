// Package verbs implements native Go transaction builders for the
// orchestrator's bloating scenarios. Each verb is a pure function that, given
// a per-tx index and a build context, returns an unsigned EIP-1559 transaction
// template (*types.DynamicFeeTx) with To, Value, Data and Gas populated.
//
// The dispatcher fills the remaining fields (ChainID, Nonce, GasFeeCap,
// GasTipCap) and hands the result to the signer. This package replaces the
// former out-of-process Python builder pool: building is now an in-process
// loop over BuildTx.
//
// The verb construction logic is a faithful port of the EELS spamoor builders
// and the orchestrator-py facade wrappers; byte-equality with the Python
// oracle is enforced by verbs_golden_test.go.
package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// BuildCtx carries the per-run immutable parameters a verb may need to build a
// transaction template. It mirrors the subset of facade.Context that the
// builders consume.
type BuildCtx struct {
	// ChainID is informational; the dispatcher sets DynamicFeeTx.ChainID.
	ChainID *big.Int
	// SignerAddr is the master signer address — the recipient for noop.
	SignerAddr common.Address
	// BaseAddress is the 20-byte base for sequential address derivation.
	BaseAddress []byte
	// Revision is the address-space generation used by derived addresses.
	Revision uint64
	// AddressStride is the per-revision stride (typically 1<<40).
	AddressStride uint64
	// SaltBase is the reserved CREATE2 salt range start for this batch.
	SaltBase uint64
}

// Verb builds unsigned transaction templates for one scenario.
type Verb interface {
	// Name returns the verb's registry key.
	Name() string
	// BuildTx returns the unsigned EIP-1559 template for transaction idx.
	BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error)
}

// Registry maps verb names to their native implementation. Verbs that are not
// yet golden-verified are intentionally absent so the dispatcher fails loudly
// rather than shipping unverified on-chain state.
var Registry = map[string]Verb{
	verbEoatx{}.Name():           verbEoatx{},
	verbNoop{}.Name():            verbNoop{},
	verbCalltx{}.Name():          verbCalltx{},
	verbDeploytx{}.Name():        verbDeploytx{},
	verbFactoryDeploytx{}.Name(): verbFactoryDeploytx{},
	verbStorageSpam{}.Name():     verbStorageSpam{},
	verbErc20Bloater{}.Name():    verbErc20Bloater{},
	verbErc20tx{}.Name():         verbErc20tx{},
	verbUniswapSwaps{}.Name():    verbUniswapSwaps{},
	verbStorageRefundtx{}.Name(): verbStorageRefundtx{},
	verbGasburnertx{}.Name():     verbGasburnertx{},
	verbEvmFuzz{}.Name():         verbEvmFuzz{},
	verbBlobCombined{}.Name():    verbBlobCombined{},
}

// Lookup returns the verb registered under name, or false if unknown.
func Lookup(name string) (Verb, bool) {
	v, ok := Registry[name]
	return v, ok
}
