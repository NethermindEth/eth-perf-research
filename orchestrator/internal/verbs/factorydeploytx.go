package verbs

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// factoryInitCode is the bytecode the factory CREATE2-deploys: PUSH1 0x01;
// PUSH1 0x00; SSTORE — writes one storage slot on deploy.
var factoryInitCode = mustHex("6001600055")

// factoryDeploySelector = keccak("deploy(bytes32,bytes)")[:4].
var factoryDeploySelector = []byte{0x4c, 0x8c, 0x9e, 0xa1}

// factoryDeployGas is the exec-tx gas limit (EELS default: 500_000).
const factoryDeployGas = 500_000

// verbFactoryDeploytx builds a CREATE2 factory call: deploy(bytes32 salt,
// bytes initCode). The per-tx index is added to the batch-reserved SaltBase
// so each tx gets a distinct salt and therefore a distinct CREATE2 deployment
// address.
type verbFactoryDeploytx struct{}

func (verbFactoryDeploytx) Name() string { return "factorydeploytx" }

func (verbFactoryDeploytx) BuildTx(idx uint64, ctx BuildCtx) (*types.DynamicFeeTx, error) {
	to := addrFactoryDeploytx
	data := encodeFactoryDeployCall(ctx.SaltBase+idx, factoryInitCode)
	return &types.DynamicFeeTx{
		To:    &to,
		Value: new(big.Int),
		Data:  data,
		Gas:   factoryDeployGas,
	}, nil
}

// encodeFactoryDeployCall ABI-encodes deploy(bytes32 salt, bytes initCode).
// Layout: selector || salt(32) || dataOffset(32=0x40) || dataLen(32) ||
// initCode right-padded to a 32-byte boundary.
func encodeFactoryDeployCall(salt uint64, initCode []byte) []byte {
	const dataOffset = 64
	length := len(initCode)
	padLen := (32 - (length % 32)) % 32
	padded := concat(initCode, make([]byte, padLen))
	return concat(
		factoryDeploySelector,
		word32(salt),
		word32(dataOffset),
		word32(uint64(length)),
		padded,
	)
}
