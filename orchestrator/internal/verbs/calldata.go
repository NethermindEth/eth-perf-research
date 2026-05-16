package verbs

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// selector returns the 4-byte ABI function selector for a Solidity-style
// signature, e.g. "transferMint(address,uint256)".
func selector(sig string) []byte {
	return crypto.Keccak256([]byte(sig))[:4]
}

// word32 left-pads v into a 32-byte big-endian ABI word.
func word32(v uint64) []byte {
	w := make([]byte, 32)
	binary.BigEndian.PutUint64(w[24:], v)
	return w
}

// wordBig left-pads a non-negative big-endian byte slice into a 32-byte ABI
// word. Input longer than 32 bytes is taken from its low 32 bytes.
func wordBig(b []byte) []byte {
	w := make([]byte, 32)
	if len(b) > 32 {
		b = b[len(b)-32:]
	}
	copy(w[32-len(b):], b)
	return w
}

// addressWord left-pads a 20-byte address into a 32-byte ABI word.
func addressWord(a common.Address) []byte {
	w := make([]byte, 32)
	copy(w[12:], a[:])
	return w
}

// concat joins byte slices into a single fresh slice.
func concat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
