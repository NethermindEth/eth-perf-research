// lifecycle_worker is a test stub for the integration smoke test.
// It reads framed BuildBatchRequest protos from stdin and writes framed
// BuildBatchResponse protos to stdout, returning synthetic EOA-transfer TxIns.
//
// Each TxIn:
//   - chain_id  = req.ctx.chain_id
//   - nonce     = req.start_idx + i
//   - gas       = 21000
//   - to        = 20 bytes of 0xAA
//   - value     = big-endian 0x01
//   - data      = empty
//   - fee fields left zero (lifecycle fills them)
//
// new_salt_cursor is echoed back unchanged.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/proto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

var toAddr = bytes20(0xAA)

func bytes20(b byte) []byte {
	out := make([]byte, 20)
	for i := range out {
		out[i] = b
	}
	return out
}

func main() {
	for {
		req, err := readRequest(os.Stdin)
		if err != nil {
			if err == io.EOF {
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "lifecycle_worker: read request: %v\n", err)
			os.Exit(1)
		}

		count := int(req.Count)
		if count <= 0 {
			count = 1
		}

		var chainID uint64
		var saltCursor uint64
		if req.Ctx != nil {
			chainID = req.Ctx.ChainId
			saltCursor = req.Ctx.SaltCursor
		}

		txs := make([]*orchpb.TxIn, count)
		for i := range count {
			txs[i] = &orchpb.TxIn{
				ChainId: chainID,
				Nonce:   req.StartIdx + uint64(i),
				Gas:     21000,
				To:      toAddr,
				Value:   []byte{0x01},
				Data:    nil,
			}
		}

		resp := &orchpb.BuildBatchResponse{
			Id:            req.Id,
			Signables:     txs,
			NewSaltCursor: saltCursor,
		}
		if err := writeResponse(os.Stdout, resp); err != nil {
			fmt.Fprintf(os.Stderr, "lifecycle_worker: write response: %v\n", err)
			os.Exit(1)
		}
	}
}

func readRequest(r io.Reader) (*orchpb.BuildBatchRequest, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var req orchpb.BuildBatchRequest
	if err := proto.Unmarshal(buf, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func writeResponse(w io.Writer, resp *orchpb.BuildBatchResponse) error {
	data, err := proto.Marshal(resp)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
