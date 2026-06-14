package facade

import (
	"math/big"
	"sync/atomic"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// Context is the per-run mutable state owned by the orchestrator.
// It is passed to Dispatcher.Dispatch for each batch.
//
// AddressCursor and SaltCursor are atomic so cursor reservation stays a single
// race-free operation; the lifecycle reserves disjoint ranges via
// ReserveAddresses / ReserveSalts. All other fields are written once during run
// setup and treated as read-only thereafter.
type Context struct {
	BaseAddress   []byte // 20-byte base for sequential address derivation
	Revision      uint64 // address-space generation
	AddressStride uint64 // typically 1<<40
	ChainID       uint64
	// BlockGasLimit is refreshed by the lifecycle every batch from the head
	// block. Accessed via the atomic wrapper so the planner loop and the
	// background fee refresher don't race on plain reads/writes.
	BlockGasLimit atomic.Uint64

	// Cursors — reserved via the ReserveAddresses / ReserveSalts methods.
	// Direct field access is intentionally non-atomic-safe; callers must use
	// the helper methods.
	AddressCursor atomic.Uint64 // == nonce of the next tx to sign
	SaltCursor    atomic.Uint64 // CREATE2 salt for factorydeploytx

	// Fee policy — set by the lifecycle from the latest block's baseFeePerGas.
	// Accessed via the typed atomic.Pointer wrappers below so the background
	// fee refresher and the planner loop don't race on the underlying
	// *big.Int. Direct field access is intentionally non-atomic; callers must
	// use SetFeePolicy / loadFeePolicy.
	maxFeePerGas         atomic.Pointer[big.Int]
	maxPriorityFeePerGas atomic.Pointer[big.Int]

	// Contracts holds the Spamoor scenario contract addresses the bootstrap
	// phase deployed. Set once after bootstrap, before the hot loop starts;
	// read-only thereafter. Threaded into every verb's BuildCtx so contract-
	// calling verbs target the deployed contracts, not dead placeholders.
	Contracts *verbs.ContractRegistry
}

func (c *Context) ReserveAddresses(n uint64) (start uint64) {
	return c.AddressCursor.Add(n) - n
}

func (c *Context) LoadAddressCursor() uint64 {
	return c.AddressCursor.Load()
}

// ReserveSalts reserves n consecutive salt slots and returns the first one.
// The salt domain is 2^64 so any unused reservations from trimming are
// irrelevant.
func (c *Context) ReserveSalts(n uint64) (start uint64) {
	return c.SaltCursor.Add(n) - n
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

func (c *Context) SetBlockGasLimit(v uint64) {
	c.BlockGasLimit.Store(v)
}

func (c *Context) LoadBlockGasLimit() uint64 {
	return c.BlockGasLimit.Load()
}
