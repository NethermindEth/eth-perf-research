// Package payloads reads and writes the length-prefixed ExecutionPayloadV3 RLP log.
package payloads

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

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

// SkipToBlock fast-forwards past every frame whose block number is <= head,
// decoding ONLY each frame's block number and seeking past the
// transaction-bearing remainder of the body. On return the reader is positioned
// at the first frame with number > head (or at EOF). Orders of magnitude cheaper
// than calling Next per skipped frame, because the (large) transaction list of
// each skipped block is never read or decoded — only the ~480-byte prefix that
// precedes BlockNumber is touched, the rest is an lseek.
func (r *Reader) SkipToBlock(head uint64) (skipped int, err error) {
	var hdr [4]byte
	for {
		start, serr := r.f.Seek(0, io.SeekCurrent)
		if serr != nil {
			return skipped, fmt.Errorf("payloads: skip seek: %w", serr)
		}
		_, rerr := io.ReadFull(r.f, hdr[:])
		if rerr == io.EOF {
			return skipped, nil
		}
		if rerr != nil {
			return skipped, fmt.Errorf("payloads: skip read header: %w", rerr)
		}
		size := binary.BigEndian.Uint32(hdr[:])

		// The six fields before BlockNumber are fixed-size (32+20+32+32+256+32
		// bytes plus RLP prefixes and the outer-list header), well under 512.
		const peekMax = 512
		peekN := int(size)
		if peekN > peekMax {
			peekN = peekMax
		}
		peek := make([]byte, peekN)
		if _, perr := io.ReadFull(r.f, peek); perr != nil {
			return skipped, fmt.Errorf("payloads: skip read peek at %d: %w", start, perr)
		}
		num, nerr := peekBlockNumber(peek)
		if nerr != nil {
			return skipped, fmt.Errorf("payloads: skip peek number at %d: %w", start, nerr)
		}
		if num > head {
			// Rewind so the caller's Next reads this frame in full.
			if _, sErr := r.f.Seek(start, io.SeekStart); sErr != nil {
				return skipped, fmt.Errorf("payloads: skip rewind: %w", sErr)
			}
			return skipped, nil
		}
		if rem := int64(size) - int64(peekN); rem > 0 {
			if _, sErr := r.f.Seek(rem, io.SeekCurrent); sErr != nil {
				return skipped, fmt.Errorf("payloads: skip advance: %w", sErr)
			}
		}
		skipped++
	}
}

// peekBlockNumber reads the 7th RLP element (BlockNumber) of an rlpPayload body
// from a prefix buffer that may be truncated mid-body. It walks element HEADERS
// only (never requiring the full outer-list content to be present), stepping past
// the six fixed-size leading fields, so it works on a ~512-byte peek of a
// multi-megabyte frame. A full rlp.Stream can't: it caps its input limit at the
// buffer length, and the outer list header declares the whole (absent) body.
func peekBlockNumber(peek []byte) (uint64, error) {
	// Enter the outer list: advance past its header to the first element.
	hdr, _, err := rlpHeader(peek)
	if err != nil {
		return 0, err
	}
	c := hdr
	// Skip the six fixed-size fields preceding BlockNumber.
	for i := 0; i < 6; i++ {
		h, p, err := rlpHeader(peek[c:])
		if err != nil {
			return 0, err
		}
		c += h + p
	}
	// Decode the seventh element (BlockNumber) as a big-endian RLP integer.
	h, p, err := rlpHeader(peek[c:])
	if err != nil {
		return 0, err
	}
	start, end := c+h, c+h+p
	if end > len(peek) {
		return 0, io.ErrUnexpectedEOF
	}
	var n uint64
	for _, b := range peek[start:end] {
		n = n<<8 | uint64(b)
	}
	return n, nil
}

// rlpHeader returns the header length and payload length of the single RLP
// element at the front of buf. For a single-byte value (< 0x80) the header is 0
// and the payload length 1 (the byte is its own value).
func rlpHeader(buf []byte) (hdrLen, payloadLen int, err error) {
	if len(buf) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	b := buf[0]
	switch {
	case b < 0x80: // single byte
		return 0, 1, nil
	case b < 0xb8: // short string
		return 1, int(b - 0x80), nil
	case b < 0xc0: // long string: length-of-length = b-0xb7
		ll := int(b - 0xb7)
		if len(buf) < 1+ll {
			return 0, 0, io.ErrUnexpectedEOF
		}
		return 1 + ll, beInt(buf[1 : 1+ll]), nil
	case b < 0xf8: // short list
		return 1, int(b - 0xc0), nil
	default: // long list: length-of-length = b-0xf7
		ll := int(b - 0xf7)
		if len(buf) < 1+ll {
			return 0, 0, io.ErrUnexpectedEOF
		}
		return 1 + ll, beInt(buf[1 : 1+ll]), nil
	}
}

// beInt reads a big-endian unsigned integer from b as an int.
func beInt(b []byte) int {
	n := 0
	for _, x := range b {
		n = n<<8 | int(x)
	}
	return n
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
		if err == io.EOF {
			return nil, io.EOF
		}
		// Wrap ErrUnexpectedEOF so NextFollow can distinguish a partially-written
		// frame (writer mid-append) from a genuine decode error.
		return nil, fmt.Errorf("payloads: truncated length header: %w", io.ErrUnexpectedEOF)
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

// NextFollow behaves like Next but tails a file that a concurrent writer is still
// appending to: on a clean EOF or a partially-written frame it rewinds to the
// frame start, waits poll, and retries — so a replay can stream blocks as a live
// bloat run records them, never stopping at the current end of file. Only a real
// decode/I/O error or ctx cancellation returns. It never returns io.EOF.
func (r *Reader) NextFollow(ctx context.Context, poll time.Duration) (*ExecutionPayloadV3, error) {
	for {
		start, err := r.f.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("payloads: follow seek: %w", err)
		}
		p, err := r.Next()
		switch {
		case err == nil:
			return p, nil
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			// End of file, or the writer is mid-frame. Rewind to the frame start so
			// the partial bytes are re-read once complete, then wait for more data.
			if _, serr := r.f.Seek(start, io.SeekStart); serr != nil {
				return nil, fmt.Errorf("payloads: follow rewind: %w", serr)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(poll):
			}
		default:
			return nil, err
		}
	}
}

func (r *Reader) Close() error {
	return r.f.Close()
}
