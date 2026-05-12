// echo_worker is a test stub that reads framed BuildBatchRequest protos from
// stdin and writes framed BuildBatchResponse protos to stdout.
//
// Flags:
//
//	--die-after-n=N  exit(1) after successfully echoing N requests (0 = never)
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/proto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

func main() {
	dieAfter := flag.Int("die-after-n", 0, "exit after N responses (0 = run forever)")
	flag.Parse()

	count := 0
	for {
		req, err := readRequest(os.Stdin)
		if err != nil {
			if err == io.EOF {
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "echo_worker: read request: %v\n", err)
			os.Exit(1)
		}

		resp := &orchpb.BuildBatchResponse{
			Id: req.Id,
			Signables: []*orchpb.TxIn{
				{Nonce: req.StartIdx},
			},
		}
		if err := writeResponse(os.Stdout, resp); err != nil {
			fmt.Fprintf(os.Stderr, "echo_worker: write response: %v\n", err)
			os.Exit(1)
		}

		count++
		if *dieAfter > 0 && count >= *dieAfter {
			fmt.Fprintf(os.Stderr, "echo_worker: dying after %d requests\n", count)
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
