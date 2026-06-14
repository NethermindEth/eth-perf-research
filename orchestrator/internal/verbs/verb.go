// Package verbs implements native Go transaction builders for the
// orchestrator's bloating scenarios. Each verb is a pure function that, given
// a per-tx index and a build context, returns an unsigned EIP-1559 transaction
// template (*types.DynamicFeeTx) with To, Value, Data and Gas populated.
//
// The dispatcher fills the remaining fields (ChainID, Nonce, GasFeeCap,
// GasTipCap) and hands the result to the signer. Byte-equality with the EELS
// Spamoor builders is enforced by verbs_golden_test.go.
package verbs

import (
	"github.com/ethereum/go-ethereum/core/types"
)

// BuildCtx carries the per-run immutable parameters a verb may need to build a
// transaction template. It mirrors the subset of facade.Context that the
// builders consume.
type BuildCtx struct {
	// BaseAddress is the 20-byte base for sequential address derivation.
	BaseAddress []byte
	// Revision is the address-space generation used by derived addresses.
	Revision uint64
	// AddressStride is the per-revision stride (typically 1<<40).
	AddressStride uint64
	// SaltBase is the reserved CREATE2 salt range start for this batch.
	SaltBase uint64
	// Contracts holds the addresses of the Spamoor scenario contracts the
	// bootstrap phase deployed. Contract-calling verbs read their target
	// address from here; it is nil only before bootstrap completes (verbs
	// are never built in that window).
	Contracts *ContractRegistry
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
var Registry = buildRegistry()

// buildRegistry assembles the verb registry from the bespoke verbs plus the
// table-driven contract-call verbs (see calltable.go). Verbs that are not yet
// golden-verified are intentionally absent so the dispatcher fails loudly
// rather than shipping unverified on-chain state.
func buildRegistry() map[string]Verb {
	reg := map[string]Verb{
		verbEoatx{}.Name():           verbEoatx{},
		verbDeploytx{}.Name():        verbDeploytx{},
		verbFactoryDeploytx{}.Name(): verbFactoryDeploytx{},
		verbErc20tx{}.Name():         verbErc20tx{},
		verbUniswapSwaps{}.Name():    verbUniswapSwaps{},
	}
	for _, spec := range callSpecs {
		reg[spec.name] = callVerb{spec: spec}
	}
	return reg
}

// Lookup returns the verb registered under name, or false if unknown.
func Lookup(name string) (Verb, bool) {
	v, ok := Registry[name]
	return v, ok
}
