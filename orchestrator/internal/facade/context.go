package facade

import (
	"math/big"
	"sync/atomic"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

// Context is the per-run mutable state owned by the orchestrator.
// It is passed to Dispatcher.Dispatch for each batch.
//
// AddressCursor and SaltCursor are atomic so multiple planner goroutines may
// reserve disjoint ranges concurrently via ReserveAddresses / ReserveSalts.
// All other fields are written once during run setup and treated as read-only
// thereafter.
type Context struct {
	BaseAddress   []byte // 20-byte master signer address
	Revision      uint64 // address-space generation
	AddressStride uint64 // typically 1<<40
	ChainID       uint64
	GasLimit      uint64
	// BlockGasLimit is refreshed by the lifecycle every batch from the head
	// block. Accessed via the atomic wrapper so concurrent planners and the
	// commit goroutine don't race on plain reads/writes.
	BlockGasLimit atomic.Uint64

	// Cursors — concurrently reserved by planner goroutines via the
	// ReserveAddresses / ReserveSalts methods. Direct field access is
	// intentionally non-atomic-safe; callers must use the helper methods.
	AddressCursor atomic.Uint64 // == nonce of the next tx to sign
	SaltCursor    atomic.Uint64 // CREATE2 salt for factorydeploytx

	// Fee policy — set by the lifecycle from the latest block's baseFeePerGas.
	// Accessed via the typed atomic.Pointer wrappers below so multiple planner
	// goroutines may refresh them concurrently without a data race on the
	// underlying *big.Int. Direct field access is intentionally non-atomic;
	// callers must use SetFeePolicy / loadFeePolicy.
	maxFeePerGas         atomic.Pointer[big.Int]
	maxPriorityFeePerGas atomic.Pointer[big.Int]

	// Per-verb gas-aware sizing.
	VerbGasFactors map[string]float64
}

// ReserveAddresses atomically reserves n consecutive address-cursor slots and
// returns the first one. Safe for concurrent callers.
func (c *Context) ReserveAddresses(n uint64) (start uint64) {
	return c.AddressCursor.Add(n) - n
}

// LoadAddressCursor returns the current high-water mark of reserved addresses.
func (c *Context) LoadAddressCursor() uint64 {
	return c.AddressCursor.Load()
}

// ReserveSalts atomically reserves n consecutive salt slots and returns the
// first one. Safe for concurrent callers. The salt domain is 2^64 so any
// unused reservations from trimming are irrelevant.
func (c *Context) ReserveSalts(n uint64) (start uint64) {
	return c.SaltCursor.Add(n) - n
}

// LoadSaltCursor returns the current high-water mark of reserved salts.
func (c *Context) LoadSaltCursor() uint64 {
	return c.SaltCursor.Load()
}

// SetFeePolicy atomically updates the EIP-1559 fee policy. Callers should
// pass freshly cloned *big.Int values to avoid aliasing.
func (c *Context) SetFeePolicy(maxFee, maxPriority *big.Int) {
	c.maxFeePerGas.Store(maxFee)
	c.maxPriorityFeePerGas.Store(maxPriority)
}

// LoadFeePolicy returns the current EIP-1559 fee policy. Returned values may
// be nil if SetFeePolicy hasn't been called yet; callers should treat nil as
// the zero big.Int.
func (c *Context) LoadFeePolicy() (maxFee, maxPriority *big.Int) {
	return c.maxFeePerGas.Load(), c.maxPriorityFeePerGas.Load()
}

// SetBlockGasLimit atomically updates the per-block gas ceiling.
func (c *Context) SetBlockGasLimit(v uint64) {
	c.BlockGasLimit.Store(v)
}

// LoadBlockGasLimit returns the current per-block gas ceiling.
func (c *Context) LoadBlockGasLimit() uint64 {
	return c.BlockGasLimit.Load()
}

// ToProto converts the immutable fields of Context into the wire representation
// sent to builder workers. The salt cursor is serialised from the current
// load value; callers that have already reserved a specific salt range
// should override req.Ctx.SaltCursor with the reservation start before send.
func (c *Context) ToProto() *orchpb.FacadeCtxParams {
	factors := make(map[string]float64, len(c.VerbGasFactors))
	for k, v := range c.VerbGasFactors {
		factors[k] = v
	}
	return &orchpb.FacadeCtxParams{
		BaseAddress:    append([]byte(nil), c.BaseAddress...),
		Revision:       c.Revision,
		ChainId:        c.ChainID,
		BlockGasLimit:  c.LoadBlockGasLimit(),
		SaltCursor:     c.LoadSaltCursor(),
		VerbGasFactors: factors,
		AddressStride:  c.AddressStride,
	}
}
