package verbs

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageBurnerRuntimeHex is the runtime bytecode of the storage burner —
// SLOAD slot 0 (counter), 16 unrolled SSTOREs at counter+1..counter+16, then
// SSTORE slot 0 = counter+16, STOP. 104 bytes.
//
// Ported verbatim from the Python verbs.py _STORAGE_BURNER_RUNTIME so the
// deployed contract is functionally identical to the lab-genesis storage-spam
// contract (call-compatible with the storage-spam selectors).
const storageBurnerRuntimeHex = "" +
	"6000546001018080556001018080556001018080556001018080556001018080" +
	"556001018080556001018080556001018080556001018080556001018080556001" +
	"018080556001018080556001018080556001018080556001018080556001018080" +
	"558060005500"

var storageBurnerRuntime = mustHex(storageBurnerRuntimeHex)

// deploytxGas is the creation-tx gas limit, matching Python verbs.py
// _deploytx_build. Leaves headroom over the ~80 k the CREATE + code deposit
// for a 138-byte runtime typically consumes.
const deploytxGas = 300_000

// buildUniqueStorageBurnerInit returns CREATE-tx init code that deploys a
// per-idx unique storage burner. Ported from Python verbs.py
// _build_unique_storage_burner_init.
//
// Prefixed with PUSH32 idx; POP so each deploy emits a distinct codehash —
// the plugin's codeBytesTotal is deduplicated by hash, so without the unique
// tag every deploytx would collapse onto a single hash and the code axis
// wouldn't grow proportionally. The EELS default init (0x6001600055) writes
// one storage slot but never RETURNs, so the deployed runtime is empty — the
// resulting account has no code, NM does not count it as a contract, and
// neither contractsTotal nor codeBytesTotal moves. The CODECOPY+RETURN
// wrapper below is what makes the CREATE actually deposit runtime code.
func buildUniqueStorageBurnerInit(idx uint64) []byte {
	// unique_tag: PUSH32 <32-byte idx>; POP — 34 bytes. idx goes in the low
	// 8 bytes of the 32-byte PUSH32 slot; the high 24 are zero.
	tag := make([]byte, 34)
	tag[0] = 0x7F // PUSH32
	binary.BigEndian.PutUint64(tag[25:33], idx)
	tag[33] = 0x50 // POP

	runtime := make([]byte, 0, len(tag)+len(storageBurnerRuntime))
	runtime = append(runtime, tag...)
	runtime = append(runtime, storageBurnerRuntime...)

	if len(runtime) > 0xFFFF {
		// PUSH2 can encode at most 0xFFFF; surfaces a programming error if
		// the runtime ever grows large enough to overflow it.
		panic("verbs: runtime too large for PUSH2 length")
	}
	n := uint16(len(runtime))

	// init_prefix (14 bytes): copy runtime from code offset 14 to memory 0
	// and RETURN it. Same shape as Python verbs.py init_prefix.
	//   PUSH2 n; PUSH1 14; PUSH1 0; CODECOPY; PUSH2 n; PUSH1 0; RETURN
	init := []byte{
		0x61, byte(n >> 8), byte(n), // PUSH2 n
		0x60, 0x0E, // PUSH1 14 (len of init prefix)
		0x60, 0x00, // PUSH1 0
		0x39,                        // CODECOPY
		0x61, byte(n >> 8), byte(n), // PUSH2 n
		0x60, 0x00, // PUSH1 0
		0xF3, // RETURN
	}
	return append(init, runtime...)
}

// verbDeploytx builds a contract-creation tx. The init code is a per-idx
// unique storage-burner ported from Python verbs.py — see
// buildUniqueStorageBurnerInit. This replaces the EELS default
// (0x6001600055) which deposits empty runtime and never grows contractsTotal
// or codeBytesTotal regardless of how many deploytx txs run.
type verbDeploytx struct{}

func (verbDeploytx) Name() string { return "deploytx" }

func (verbDeploytx) BuildTx(idx uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	return &types.DynamicFeeTx{
		To:    nil, // contract creation
		Value: new(big.Int),
		Data:  buildUniqueStorageBurnerInit(idx),
		Gas:   deploytxGas,
	}, nil
}

// mustHex decodes a compile-time hex constant or panics — used only for
// statically-known byte arrays ported from the EELS builders.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(errors.New("verbs: invalid hex constant: " + s))
	}
	return b
}
