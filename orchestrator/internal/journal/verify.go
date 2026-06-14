package journal

import (
	"errors"
	"fmt"
	"io"
)

// VerifyAll reads the journal at path, walks the chain-hash sequence, and
// returns the record count and final chain hash.
// Returns ErrChainHashMismatch on the first broken link.
func VerifyAll(path string) (recordCount int, finalHash [32]byte, err error) {
	r, err := OpenReader(path)
	if err != nil {
		return 0, zeroHash, fmt.Errorf("journal: verify-all open: %w", err)
	}
	defer r.Close()

	for {
		_, rerr := r.Next()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return recordCount, zeroHash, rerr
		}
		recordCount++
	}
	return recordCount, r.prevHash, nil
}
