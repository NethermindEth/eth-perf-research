// Package payloads reads and writes the length-prefixed ExecutionPayloadV3 RLP log.
package payloads

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// ExecutionPayloadV3 aliases go-ethereum's engine.ExecutableData. The on-disk
// wire format (rlpPayload + 4-byte BE length prefix) is preserved for
// byte-equal compatibility with existing payloads.rlp artifacts.
type ExecutionPayloadV3 = engine.ExecutableData

// rlpPayload is the wire representation fed to rlp.Encode.
// go-ethereum's RLP encoder uses struct field order; types.Withdrawal has its
// own RLP encoding that matches the spec (Index, Validator, Address, Amount).
type rlpPayload struct {
	ParentHash    []byte
	FeeRecipient  []byte
	StateRoot     []byte
	ReceiptsRoot  []byte
	LogsBloom     []byte
	PrevRandao    []byte
	BlockNumber   uint64
	GasLimit      uint64
	GasUsed       uint64
	Timestamp     uint64
	ExtraData     []byte
	BaseFeePerGas *big.Int
	BlockHash     []byte
	Transactions  [][]byte
	Withdrawals   []*types.Withdrawal
	BlobGasUsed   uint64
	ExcessBlobGas uint64
}

func toRLP(p *ExecutionPayloadV3) *rlpPayload {
	if len(p.LogsBloom) != 256 {
		// All callers (lifecycle, heal-payloads, replay) build LogsBloom from
		// a 256-byte source; this guards a future caller that forgets.
		panic(fmt.Sprintf("payloads: LogsBloom must be 256 bytes, got %d", len(p.LogsBloom)))
	}
	blobGasUsed := uint64(0)
	if p.BlobGasUsed != nil {
		blobGasUsed = *p.BlobGasUsed
	}
	excessBlobGas := uint64(0)
	if p.ExcessBlobGas != nil {
		excessBlobGas = *p.ExcessBlobGas
	}
	return &rlpPayload{
		ParentHash:    p.ParentHash[:],
		FeeRecipient:  p.FeeRecipient[:],
		StateRoot:     p.StateRoot[:],
		ReceiptsRoot:  p.ReceiptsRoot[:],
		LogsBloom:     p.LogsBloom,
		PrevRandao:    p.Random[:],
		BlockNumber:   p.Number,
		GasLimit:      p.GasLimit,
		GasUsed:       p.GasUsed,
		Timestamp:     p.Timestamp,
		ExtraData:     p.ExtraData,
		BaseFeePerGas: p.BaseFeePerGas,
		BlockHash:     p.BlockHash[:],
		Transactions:  p.Transactions,
		Withdrawals:   p.Withdrawals,
		BlobGasUsed:   blobGasUsed,
		ExcessBlobGas: excessBlobGas,
	}
}

func fromRLP(r *rlpPayload) (*ExecutionPayloadV3, error) {
	p := &ExecutionPayloadV3{
		Number:        r.BlockNumber,
		GasLimit:      r.GasLimit,
		GasUsed:       r.GasUsed,
		Timestamp:     r.Timestamp,
		ExtraData:     r.ExtraData,
		BaseFeePerGas: r.BaseFeePerGas,
		Transactions:  r.Transactions,
		Withdrawals:   r.Withdrawals,
		BlobGasUsed:   &r.BlobGasUsed,
		ExcessBlobGas: &r.ExcessBlobGas,
	}
	if n := copy(p.ParentHash[:], r.ParentHash); n != 32 {
		return nil, fmt.Errorf("payloads: parentHash: expected 32 bytes, got %d", n)
	}
	if n := copy(p.FeeRecipient[:], r.FeeRecipient); n != 20 {
		return nil, fmt.Errorf("payloads: feeRecipient: expected 20 bytes, got %d", n)
	}
	if n := copy(p.StateRoot[:], r.StateRoot); n != 32 {
		return nil, fmt.Errorf("payloads: stateRoot: expected 32 bytes, got %d", n)
	}
	if n := copy(p.ReceiptsRoot[:], r.ReceiptsRoot); n != 32 {
		return nil, fmt.Errorf("payloads: receiptsRoot: expected 32 bytes, got %d", n)
	}
	if len(r.LogsBloom) != 256 {
		return nil, fmt.Errorf("payloads: logsBloom: expected 256 bytes, got %d", len(r.LogsBloom))
	}
	p.LogsBloom = append([]byte(nil), r.LogsBloom...)
	if n := copy(p.Random[:], r.PrevRandao); n != 32 {
		return nil, fmt.Errorf("payloads: prevRandao: expected 32 bytes, got %d", n)
	}
	if n := copy(p.BlockHash[:], r.BlockHash); n != 32 {
		return nil, fmt.Errorf("payloads: blockHash: expected 32 bytes, got %d", n)
	}
	return p, nil
}

// Writer appends length-prefixed RLP payloads to a file, fsync on Close.
type Writer struct {
	f *os.File
}

func OpenWriter(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("payloads: open writer: %w", err)
	}
	return &Writer{f: f}, nil
}

// Append RLP-encodes p and writes a 4-byte BE length prefix followed by the body.
func (w *Writer) Append(p *ExecutionPayloadV3) error {
	var buf bytes.Buffer
	if err := rlp.Encode(&buf, toRLP(p)); err != nil {
		return fmt.Errorf("payloads: rlp encode: %w", err)
	}
	body := buf.Bytes()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.f.Write(hdr[:]); err != nil {
		return fmt.Errorf("payloads: write header: %w", err)
	}
	if _, err := w.f.Write(body); err != nil {
		return fmt.Errorf("payloads: write body: %w", err)
	}
	return nil
}

func (w *Writer) Sync() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("payloads: fsync: %w", err)
	}
	return nil
}

// Close fsyncs and closes the underlying file.
func (w *Writer) Close() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("payloads: fsync: %w", err)
	}
	return w.f.Close()
}

// AppendRaw writes a pre-framed record (4-byte length prefix + body) verbatim.
// Used to copy an existing frame byte-for-byte without re-encoding.
func (w *Writer) AppendRaw(frame []byte) error {
	if _, err := w.f.Write(frame); err != nil {
		return fmt.Errorf("payloads: write raw frame: %w", err)
	}
	return nil
}

// FrameRef locates one frame within a payloads file: its block number, the
// byte offset of its 4-byte length header, and the total frame length
// (4 + body).
type FrameRef struct {
	Number uint64
	Offset int64
	Length int
}

// ScanIndex walks path and returns one FrameRef per frame in file order,
// decoding only the block number from each body. Cheap relative to a full
// decode but still reads the whole file sequentially.
func ScanIndex(path string) ([]FrameRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("payloads: scan open: %w", err)
	}
	defer f.Close()

	var (
		refs   []FrameRef
		offset int64
		hdr    [4]byte
	)
	for {
		_, err := io.ReadFull(f, hdr[:])
		if err == io.EOF {
			return refs, nil
		}
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("payloads: scan: truncated length header at offset %d", offset)
		}
		if err != nil {
			return nil, fmt.Errorf("payloads: scan read header: %w", err)
		}
		size := binary.BigEndian.Uint32(hdr[:])
		body := make([]byte, size)
		if _, err := io.ReadFull(f, body); err != nil {
			return nil, fmt.Errorf("payloads: scan: truncated body at offset %d: %w", offset, err)
		}
		var rp rlpPayload
		if err := rlp.DecodeBytes(body, &rp); err != nil {
			return nil, fmt.Errorf("payloads: scan rlp decode at offset %d: %w", offset, err)
		}
		refs = append(refs, FrameRef{Number: rp.BlockNumber, Offset: offset, Length: 4 + int(size)})
		offset += int64(4 + int(size))
	}
}

func ReadFrameAt(f io.ReaderAt, ref FrameRef) ([]byte, error) {
	buf := make([]byte, ref.Length)
	if _, err := f.ReadAt(buf, ref.Offset); err != nil {
		return nil, fmt.Errorf("payloads: read frame at %d: %w", ref.Offset, err)
	}
	return buf, nil
}

func DecodeFrame(frame []byte) (*ExecutionPayloadV3, error) {
	if len(frame) < 4 {
		return nil, fmt.Errorf("payloads: frame too short: %d bytes", len(frame))
	}
	size := binary.BigEndian.Uint32(frame[:4])
	if len(frame) != 4+int(size) {
		return nil, fmt.Errorf("payloads: frame length mismatch: header=%d actual=%d", size, len(frame)-4)
	}
	var rp rlpPayload
	if err := rlp.DecodeBytes(frame[4:], &rp); err != nil {
		return nil, fmt.Errorf("payloads: decode frame: %w", err)
	}
	return fromRLP(&rp)
}

// Reader reads length-prefixed RLP payloads sequentially from a file.
type Reader struct {
	f *os.File
}

func OpenReader(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("payloads: open reader: %w", err)
	}
	return &Reader{f: f}, nil
}

// Next reads and decodes the next payload. Returns io.EOF when the stream ends.
func (r *Reader) Next() (*ExecutionPayloadV3, error) {
	var hdr [4]byte
	_, err := io.ReadFull(r.f, hdr[:])
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// Distinguish clean EOF (0 bytes read) from mid-header truncation.
		n, _ := r.f.Seek(0, io.SeekCurrent)
		_ = n
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("payloads: truncated length header")
	}
	if err != nil {
		return nil, fmt.Errorf("payloads: read header: %w", err)
	}

	size := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, size)
	if _, err := io.ReadFull(r.f, body); err != nil {
		return nil, fmt.Errorf("payloads: truncated body: %w", err)
	}

	var rp rlpPayload
	if err := rlp.DecodeBytes(body, &rp); err != nil {
		return nil, fmt.Errorf("payloads: rlp decode: %w", err)
	}
	return fromRLP(&rp)
}

func (r *Reader) Close() error {
	return r.f.Close()
}
