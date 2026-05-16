package verbs

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// evm_fuzz tuning constants — ported verbatim from EELS build_evm_fuzz_-
// transactions / _evm_fuzz_bytecode. The facade pins payload_seed="" (so the
// seed defaults to "deadbeef"), min/max code size 100/512, fuzz_mode "all".
const (
	evmFuzzMinCodeSize = 100
	evmFuzzMaxCodeSize = 512
	evmFuzzModeAll     = 0      // mode "all"
	evmFuzzValueBase   = 0xA000 // low bound of the non-zero value range
	evmFuzzValueSpan   = 0x6000 // width of the non-zero value range
)

// evmFuzzSeedHex is the default payload_seed parsed as hex (4 bytes) — used by
// the bytecode generator. evmFuzzSeedText is the same label's raw UTF-8 bytes
// (8 bytes) — used by the value generator. The Python encodes the seed
// differently in each path, so both forms are kept.
var (
	evmFuzzSeedHex  = mustHex("deadbeef")
	evmFuzzSeedText = []byte("deadbeef")
)

// verbEvmFuzz builds a contract-creation tx carrying deterministic
// pseudo-random initcode derived from the tx index via SHA-256 counters.
type verbEvmFuzz struct{}

func (verbEvmFuzz) Name() string { return "evm_fuzz" }

func (verbEvmFuzz) BuildTx(idx uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	return &types.DynamicFeeTx{
		To:    nil, // contract creation
		Value: evmFuzzValue(idx),
		Data:  evmFuzzBytecode(idx),
		Gas:   300_000,
	}, nil
}

// evmFuzzBytecode deterministically derives fuzz initcode for tx index txID.
// Mirrors _evm_fuzz_bytecode with seed_bytes = bytes.fromhex("deadbeef").
func evmFuzzBytecode(txID uint64) []byte {
	idBE := make([]byte, 8)
	binary.BigEndian.PutUint64(idBE, txID)

	const sizeSpan = evmFuzzMaxCodeSize - evmFuzzMinCodeSize + 1
	sizeDigest := sha256.Sum256(concat(evmFuzzSeedHex, idBE, []byte("size")))
	size := evmFuzzMinCodeSize + int(binary.BigEndian.Uint32(sizeDigest[:4])%sizeSpan)

	out := make([]byte, 0, size+sha256.Size)
	for counter := uint32(0); len(out) < size; counter++ {
		ctrBE := make([]byte, 4)
		binary.BigEndian.PutUint32(ctrBE, counter)
		h := sha256.Sum256(concat(evmFuzzSeedHex, idBE, []byte{evmFuzzModeAll}, ctrBE))
		out = append(out, h[:]...)
	}
	return out[:size]
}

// evmFuzzValue returns the tx value for index txID. Every 4th tx carries
// value 0; the rest carry a deterministic value in [0xA000, 0x10000).
// Mirrors the value split in build_evm_fuzz_transactions.
func evmFuzzValue(txID uint64) *big.Int {
	if txID%4 == 0 {
		return new(big.Int)
	}
	idBE := make([]byte, 8)
	binary.BigEndian.PutUint64(idBE, txID)
	h := sha256.Sum256(concat(evmFuzzSeedText, idBE, []byte("val")))
	v := uint64(evmFuzzValueBase) + uint64(binary.BigEndian.Uint16(h[:2]))%evmFuzzValueSpan
	return new(big.Int).SetUint64(v)
}
