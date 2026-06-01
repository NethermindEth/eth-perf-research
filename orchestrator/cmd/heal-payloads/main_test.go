package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// buildBlock converts a payload into a types.Block with a real computed hash.
// Uses ExecutableDataToBlockNoHash so block.Hash() is canonical (no stored-hash
// check), then derives the payload back via BlockToExecutableData so BlockHash
// reflects what the debug fetcher will compute.
func buildBlock(p *payloads.ExecutionPayloadV3) *types.Block {
	blk, err := engine.ExecutableDataToBlockNoHash(*p, nil, nil, nil)
	if err != nil {
		panic(fmt.Sprintf("buildBlock: ExecutableDataToBlockNoHash: %v", err))
	}
	return blk
}

// buildCanonicalChain constructs n+1 payloads (indices 0..n) where each
// payload's ParentHash and BlockHash are the cryptographically correct hashes
// of the underlying types.Block chain. Blocks use deterministic but minimal
// header fields; block 0 uses common.Hash{} as parent.
func buildCanonicalChain(n uint64) []*payloads.ExecutionPayloadV3 {
	out := make([]*payloads.ExecutionPayloadV3, n+1)
	var prevHash common.Hash // genesis parent = zero hash
	for i := uint64(0); i <= n; i++ {
		var bloom [256]byte
		for j := range bloom {
			bloom[j] = byte(i) ^ byte(j)
		}
		blobGasUsed := uint64(0)
		excessBlobGas := uint64(0)
		p := &payloads.ExecutionPayloadV3{
			ParentHash:    prevHash,
			FeeRecipient:  common.Address{0xaa, byte(i)},
			StateRoot:     common.Hash{0x40, byte(i)},
			ReceiptsRoot:  common.Hash{0x60, byte(i)},
			LogsBloom:     bloom[:],
			Random:        common.Hash{0x80, byte(i)},
			Number:        i,
			GasLimit:      30_000_000,
			GasUsed:       0,
			Timestamp:     1_700_000_000 + i*12,
			ExtraData:     []byte{0xde, 0xad, byte(i)},
			BaseFeePerGas: new(big.Int).SetUint64(7 + i),
			Transactions:  [][]byte{},
			Withdrawals:   []*types.Withdrawal{{Index: i, Validator: i, Address: common.Address{0xbb, byte(i)}, Amount: 1000 + i}},
			BlobGasUsed:   &blobGasUsed,
			ExcessBlobGas: &excessBlobGas,
		}
		blk := buildBlock(p)
		ep := engine.BlockToExecutableData(blk, nil, nil, nil).ExecutionPayload
		out[i] = ep
		prevHash = blk.Hash()
	}
	return out
}

// rawBlockFor encodes a payload's underlying types.Block as RLP hex, as
// debug_getRawBlock would return from a real node.
func rawBlockFor(p *payloads.ExecutionPayloadV3) string {
	blk := buildBlock(p)
	b, err := rlp.EncodeToBytes(blk)
	if err != nil {
		panic(fmt.Sprintf("rawBlockFor: rlp encode: %v", err))
	}
	return "0x" + hex.EncodeToString(b)
}

// fakeRPC stands in for Nethermind. It indexes payloads by block number and
// serves eth_getBlockByNumber, debug_getRawBlock and eth_blockNumber.
type fakeRPC struct {
	payloadsByNum map[uint64]*payloads.ExecutionPayloadV3
	head          uint64
}

func newFakeRPC(blocks []*payloads.ExecutionPayloadV3, head uint64) *fakeRPC {
	f := &fakeRPC{
		payloadsByNum: make(map[uint64]*payloads.ExecutionPayloadV3),
		head:          head,
	}
	for _, p := range blocks {
		f.payloadsByNum[p.Number] = p
	}
	return f
}

func (f *fakeRPC) handleBlockByNumber(params []json.RawMessage) (any, *rpcErr) {
	if len(params) < 1 {
		return nil, &rpcErr{Code: -32602, Message: "missing block tag"}
	}
	var tag string
	if err := json.Unmarshal(params[0], &tag); err != nil {
		return nil, &rpcErr{Code: -32602, Message: err.Error()}
	}
	var n uint64
	if tag == "latest" {
		n = f.head
	} else {
		s := tag
		if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
			s = s[2:]
		}
		if _, err := fmt.Sscanf(s, "%x", &n); err != nil {
			return nil, &rpcErr{Code: -32602, Message: err.Error()}
		}
	}
	p, ok := f.payloadsByNum[n]
	if !ok {
		return nil, nil // returns null
	}
	return wireBlockFor(p), nil
}

func (f *fakeRPC) handleRawBlock(params []json.RawMessage) (any, *rpcErr) {
	if len(params) < 1 {
		return nil, &rpcErr{Code: -32602, Message: "missing block tag"}
	}
	var tag string
	if err := json.Unmarshal(params[0], &tag); err != nil {
		return nil, &rpcErr{Code: -32602, Message: err.Error()}
	}
	var n uint64
	s := tag
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	if _, err := fmt.Sscanf(s, "%x", &n); err != nil {
		return nil, &rpcErr{Code: -32602, Message: err.Error()}
	}
	p, ok := f.payloadsByNum[n]
	if !ok {
		return nil, &rpcErr{Code: -32602, Message: fmt.Sprintf("block %d not found", n)}
	}
	return rawBlockFor(p), nil
}

// wireBlockFor renders a payload into the JSON shape eth_getBlockByNumber
// returns, using the payload's BlockHash field as the canonical hash.
func wireBlockFor(p *payloads.ExecutionPayloadV3) map[string]any {
	withdrawals := make([]map[string]string, len(p.Withdrawals))
	for i, w := range p.Withdrawals {
		withdrawals[i] = map[string]string{
			"index":          fmt.Sprintf("0x%x", w.Index),
			"validatorIndex": fmt.Sprintf("0x%x", w.Validator),
			"address":        "0x" + hex.EncodeToString(w.Address[:]),
			"amount":         fmt.Sprintf("0x%x", w.Amount),
		}
	}
	return map[string]any{
		"number":        fmt.Sprintf("0x%x", p.Number),
		"hash":          "0x" + hex.EncodeToString(p.BlockHash[:]),
		"parentHash":    "0x" + hex.EncodeToString(p.ParentHash[:]),
		"miner":         "0x" + hex.EncodeToString(p.FeeRecipient[:]),
		"stateRoot":     "0x" + hex.EncodeToString(p.StateRoot[:]),
		"receiptsRoot":  "0x" + hex.EncodeToString(p.ReceiptsRoot[:]),
		"logsBloom":     "0x" + hex.EncodeToString(p.LogsBloom[:]),
		"mixHash":       "0x" + hex.EncodeToString(p.Random[:]),
		"gasLimit":      fmt.Sprintf("0x%x", p.GasLimit),
		"gasUsed":       fmt.Sprintf("0x%x", p.GasUsed),
		"timestamp":     fmt.Sprintf("0x%x", p.Timestamp),
		"extraData":     "0x" + hex.EncodeToString(p.ExtraData),
		"baseFeePerGas": fmt.Sprintf("0x%x", p.BaseFeePerGas),
		"blobGasUsed":   fmt.Sprintf("0x%x", deref(p.BlobGasUsed)),
		"excessBlobGas": fmt.Sprintf("0x%x", deref(p.ExcessBlobGas)),
		"withdrawals":   withdrawals,
	}
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func deref(p *uint64) uint64 {
	if p == nil {
		return 0
	}
	return *p
}

func newFakeRPCServer(t *testing.T, f *fakeRPC) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		var rerr *rpcErr
		switch req.Method {
		case "eth_blockNumber":
			result = fmt.Sprintf("0x%x", f.head)
		case "eth_getBlockByNumber":
			result, rerr = f.handleBlockByNumber(req.Params)
		case "debug_getRawBlock":
			result, rerr = f.handleRawBlock(req.Params)
		default:
			rerr = &rpcErr{Code: -32601, Message: "method not found: " + req.Method}
		}
		envelope := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			envelope["error"] = rerr
		} else {
			envelope["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writePayloadsFile(t *testing.T, path string, ps []*payloads.ExecutionPayloadV3) {
	t.Helper()
	w, err := payloads.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	for _, p := range ps {
		if err := w.Append(p); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func readPayloadNumbers(t *testing.T, path string) []uint64 {
	t.Helper()
	r, err := payloads.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()
	var out []uint64
	for {
		p, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, p.Number)
	}
}

func TestHealPayloadsFillsGap(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "payloads.rlp")
	outPath := filepath.Join(dir, "payloads.rlp.healed")

	// Build 5 payloads (indices 0..4 = blocks 1..5 after shift); we use
	// buildCanonicalChain so hashes are real and round-trip via debug_getRawBlock.
	all := buildCanonicalChain(5)
	// Write only blocks 1, 2, 5 to disk; serve 3 and 4 via the fake RPC.
	// Heal must produce a contiguous [1..5] output.
	writePayloadsFile(t, inPath, []*payloads.ExecutionPayloadV3{all[1], all[2], all[5]})

	fake := newFakeRPC(all, 5)
	srv := newFakeRPCServer(t, fake)

	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("rpc.NewClient: %v", err)
	}
	fetcher := newDebugBlockFetcher(client)

	summary, err := Heal(context.Background(), HealConfig{
		InputPath:   inPath,
		OutputPath:  outPath,
		Fetcher:     fetcher,
		IncludeTail: true,
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if summary.InputFrames != 3 {
		t.Errorf("InputFrames: got %d want 3", summary.InputFrames)
	}
	if summary.OutputFrames != 5 {
		t.Errorf("OutputFrames: got %d want 5", summary.OutputFrames)
	}
	if summary.GapsFilled != 2 {
		t.Errorf("GapsFilled: got %d want 2", summary.GapsFilled)
	}
	if summary.TailFilled != 0 {
		t.Errorf("TailFilled: got %d want 0", summary.TailFilled)
	}
	if summary.FirstBlock != 1 || summary.LastBlock != 5 {
		t.Errorf("range: got [%d..%d] want [1..5]", summary.FirstBlock, summary.LastBlock)
	}

	got := readPayloadNumbers(t, outPath)
	want := []uint64{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("block numbers: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("block at idx %d: got %d want %d", i, got[i], want[i])
		}
	}
}

func TestHealPayloadsTail(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "payloads.rlp")
	outPath := filepath.Join(dir, "payloads.rlp.healed")

	all := buildCanonicalChain(5)
	// Input has [1,2,3]; node head is 5 → tail must add 4 and 5.
	writePayloadsFile(t, inPath, all[1:4])
	fake := newFakeRPC(all, 5)
	srv := newFakeRPCServer(t, fake)
	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("rpc.NewClient: %v", err)
	}

	summary, err := Heal(context.Background(), HealConfig{
		InputPath:   inPath,
		OutputPath:  outPath,
		Fetcher:     newDebugBlockFetcher(client),
		IncludeTail: true,
	})
	if err != nil {
		t.Fatalf("Heal: %v", err)
	}
	if summary.TailFilled != 2 {
		t.Errorf("TailFilled: got %d want 2", summary.TailFilled)
	}
	got := readPayloadNumbers(t, outPath)
	if fmt.Sprint(got) != "[1 2 3 4 5]" {
		t.Fatalf("blocks: got %v want [1 2 3 4 5]", got)
	}
}
