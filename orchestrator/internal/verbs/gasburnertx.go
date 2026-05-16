package verbs

import (
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// verbGasburnertx builds a gas-burner exec call against the deployed GasBurner
// contract. Spamoor's gasburnertx scenario sends a tx whose calldata is the
// 4-byte big-endian tx index (gasburnertx.go: txIdBytes); the contract ignores
// calldata and loops burning gas until fewer than gas_remainder units remain,
// then emits one LOG1. The orchestrator threads the per-tx index into the
// 4-byte payload so each tx is distinct, matching Spamoor's wire format.
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
		Gas:   1_500_000,
	}, nil
}
