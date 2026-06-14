package verbs

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// addrSpaceBits is the width of a 20-byte Ethereum address; a derived
// recipient address that needs more bits than this cannot fit and is an error.
const addrSpaceBits = 160

// verbEoatx is THE account-trie growth verb. Each eoatx transaction sends a
// small non-zero value to a brand-new, never-before-used recipient address,
// which inserts a fresh account leaf into the state trie.
//
// This deliberately DIVERGES from EELS build_eoatx_transactions
// (helpers.py:212), which sends every tx to the zero address with value =
// amount (default 0). EELS's eoatx is a gas benchmark, so a 0-value send to
// the already-existing zero address is correct there. For a state-bloating
// orchestrator that default is a bug: a 0-value transfer to an existing
// address creates zero accounts and adds zero bytes to the account trie — it
// only bumps the sender nonce. So this verb instead derives a unique recipient
// per tx and sends 1 wei, guaranteeing exactly one new EOA per tx.
type verbEoatx struct{}

func (verbEoatx) Name() string { return "eoatx" }

// BuildTx builds an EOA-to-EOA type-2 transfer that creates the recipient.
// Value is 1 wei because a 0-value send to an empty address is a no-op and
// inserts no account leaf.
func (verbEoatx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := deriveAddress(ctx, idx)
	if err != nil {
		return nil, err
	}
	return &types.DynamicFeeTx{
		To:    &to,
		Value: big.NewInt(1),
		Data:  []byte{},
		Gas:   21_000,
	}, nil
}

// deriveAddress returns the 20-byte recipient address for logical index idx:
// baseAddress + revision*stride + idx, big-endian-packed. The revision*stride
// term keeps successive address-space generations disjoint; idx makes every
// recipient unique within a generation.
func deriveAddress(ctx BuildCtx, idx uint64) (common.Address, error) {
	base := new(big.Int)
	if len(ctx.BaseAddress) > 0 {
		base.SetBytes(ctx.BaseAddress)
	}
	stride := new(big.Int).SetUint64(ctx.AddressStride)
	rev := new(big.Int).SetUint64(ctx.Revision)
	addr := new(big.Int).Mul(rev, stride)
	addr.Add(addr, base)
	addr.Add(addr, new(big.Int).SetUint64(idx))
	if addr.BitLen() > addrSpaceBits {
		return common.Address{}, errors.New("verbs: address derivation overflows 20 bytes")
	}
	var out common.Address
	addr.FillBytes(out[:])
	return out, nil
}
