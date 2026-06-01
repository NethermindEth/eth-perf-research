package verbs

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// gasburnerGasUnitsToBurn is the exec-tx gas limit (Spamoor/EELS default
// GasUnitsToBurn).
const gasburnerGasUnitsToBurn = 2_000_000

// verbGasburnertx builds a gas-burner exec call against the deployed GasBurner
// contract. Calldata is the 4-byte big-endian tx index; the contract ignores
// calldata and loops burning gas until fewer than gas_remainder units remain,
// then emits one LOG1.
type verbGasburnertx struct{}

func (verbGasburnertx) Name() string { return "gasburnertx" }

func (v verbGasburnertx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to, err := verbTarget(ctx, v.Name())
	if err != nil {
		return nil, err
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(idx))
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  payload,
		Gas:   gasburnerGasUnitsToBurn,
	}, nil
}
