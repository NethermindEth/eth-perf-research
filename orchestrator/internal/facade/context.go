package facade

import (
	"math/big"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// Context is the per-run mutable state owned by the orchestrator.
// It is passed to Dispatcher.Dispatch for each batch.
// The only fields mutated during Dispatch are AddressCursor and SaltCursor.
type Context struct {
	BaseAddress   []byte // 20-byte master signer address
	Revision      uint64 // address-space generation
	AddressStride uint64 // typically 1<<40
	ChainID       uint64
	GasLimit      uint64
	BlockGasLimit uint64

	// Cursors — mutated per-batch by Dispatch.
	AddressCursor uint64 // == nonce of the next tx to sign
	SaltCursor    uint64 // CREATE2 salt for factorydeploytx

	// Fee policy — set by the lifecycle from the latest block's baseFeePerGas.
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int

	// Per-verb gas-aware sizing.
	VerbGasFactors map[string]float64
}

// ToProto converts the immutable fields of Context into the wire representation
// sent to builder workers. Cursors are serialised from the current values so
// workers see the up-to-date position at call time.
func (c *Context) ToProto() *orchpb.FacadeCtxParams {
	factors := make(map[string]float64, len(c.VerbGasFactors))
	for k, v := range c.VerbGasFactors {
		factors[k] = v
	}
	return &orchpb.FacadeCtxParams{
		BaseAddress:    append([]byte(nil), c.BaseAddress...),
		Revision:       c.Revision,
		ChainId:        c.ChainID,
		BlockGasLimit:  c.BlockGasLimit,
		SaltCursor:     c.SaltCursor,
		VerbGasFactors: factors,
		AddressStride:  c.AddressStride,
	}
}
