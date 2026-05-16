package verbs

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

// storageBurnerRuntime is the SLOAD-counter + 16 unrolled SSTORE storage-burner
// runtime (104 bytes). Ported verbatim from orchestrator-py verbs.py
// (_STORAGE_BURNER_RUNTIME).
var storageBurnerRuntime = mustHex(
	"6000546001018080556001018080556001018080556001018080556001018080" +
		"556001018080556001018080556001018080556001018080556001018080556001" +
		"018080556001018080556001018080556001018080556001018080556001018080" +
		"558060005500",
)

// verbDeploytx builds a contract-creation tx whose initcode deploys a per-idx
// unique storage burner. The PUSH32 idx tag makes every deploy emit a distinct
// codehash so the plugin's codeBytesTotal axis grows.
type verbDeploytx struct{}

func (verbDeploytx) Name() string { return "deploytx" }

func (verbDeploytx) BuildTx(idx uint64, _ BuildCtx) (*types.DynamicFeeTx, error) {
	initCode, err := uniqueStorageBurnerInit(idx)
	if err != nil {
		return nil, err
	}
	return &types.DynamicFeeTx{
		To:    nil, // contract creation
		Value: new(big.Int),
		Data:  initCode,
		Gas:   300_000,
	}, nil
}

// uniqueStorageBurnerInit returns CREATE-tx initcode that deploys a storage
// burner uniquely tagged with idx. Mirrors verbs.py
// _build_unique_storage_burner_init.
func uniqueStorageBurnerInit(idx uint64) ([]byte, error) {
	// PUSH32 idx; POP — a per-idx unique prefix so codehashes don't dedupe.
	uniqueTag := make([]byte, 0, 34)
	uniqueTag = append(uniqueTag, 0x7f)
	uniqueTag = append(uniqueTag, word32(idx)...)
	uniqueTag = append(uniqueTag, 0x50)

	runtime := concat(uniqueTag, storageBurnerRuntime)
	length := len(runtime)
	if length > 0xFFFF {
		return nil, fmt.Errorf("verbs: runtime too large for PUSH2 length: %d", length)
	}
	// PUSH2 length; PUSH1 14; PUSH1 0; CODECOPY; PUSH2 length; PUSH1 0; RETURN
	hi := byte(length >> 8)
	lo := byte(length)
	initPrefix := []byte{
		0x61, hi, lo,
		0x60, 0x0E,
		0x60, 0x00,
		0x39,
		0x61, hi, lo,
		0x60, 0x00,
		0xF3,
	}
	return concat(initPrefix, runtime), nil
}

// mustHex decodes a compile-time hex constant or panics — used only for
// statically-known byte arrays ported from the Python builders.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(errors.New("verbs: invalid hex constant: " + s))
	}
	return b
}
