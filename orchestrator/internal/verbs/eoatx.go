package verbs

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const addrSpaceBits = 160

// verbEoatx builds a value-1 EOA transfer to a freshly derived address, so
// every tx writes a new account-trie leaf.
type verbEoatx struct{}

func (verbEoatx) Name() string { return "eoatx" }

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

// deriveAddress returns the 20-byte address for logical index idx:
// baseAddress + revision*stride + idx, big-endian-packed. Mirrors
// orchestrator-py FacadeContext.derive_address.
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
