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
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// fakePayload synthesises a deterministic ExecutionPayloadV3 for block n.
func fakePayload(n uint64) *payloads.ExecutionPayloadV3 {
	hash := func(seed byte) common.Hash {
		var h common.Hash
		for i := range h {
			h[i] = seed
		}
		return h
	}
	var bloom [256]byte
	for i := range bloom {
		bloom[i] = byte(n) ^ byte(i)
	}
	return &payloads.ExecutionPayloadV3{
		ParentHash:    hash(byte(n - 1)),
		FeeRecipient:  common.Address{0xaa, byte(n)},
		StateRoot:     hash(byte(n) | 0x40),
		ReceiptsRoot:  hash(byte(n) | 0x60),
		LogsBloom:     bloom,
		PrevRandao:    hash(byte(n) | 0x80),
		BlockNumber:   n,
		GasLimit:      30_000_000,
		GasUsed:       21000 * n,
		Timestamp:     1_700_000_000 + n*12,
		ExtraData:     []byte{0xde, 0xad, byte(n)},
		BaseFeePerGas: new(big.Int).SetUint64(7 + n),
		BlockHash:     hash(byte(n)),
		Transactions:  [][]byte{{0x01, byte(n)}, {0x02, byte(n)}},
		Withdrawals:   []*types.Withdrawal{{Index: n, Validator: n, Address: common.Address{0xbb, byte(n)}, Amount: 1000 + n}},
		BlobGasUsed:   0,
		ExcessBlobGas: 0,
	}
}

// fakeRPC stands in for Nethermind. It indexes payloads by block number,
// derives synthetic tx hashes, and serves eth_getBlockByNumber,
// eth_getRawTransactionByHash and eth_blockNumber.
type fakeRPC struct {
	payloadsByNum map[uint64]*payloads.ExecutionPayloadV3
	txByHash      map[string][]byte
	head          uint64
}

func newFakeRPC(blocks []*payloads.ExecutionPayloadV3, head uint64) *fakeRPC {
	f := &fakeRPC{
		payloadsByNum: make(map[uint64]*payloads.ExecutionPayloadV3),
		txByHash:      make(map[string][]byte),
		head:          head,
	}
	for _, p := range blocks {
		f.payloadsByNum[p.BlockNumber] = p
		for i, tx := range p.Transactions {
			h := txHash(p.BlockNumber, i)
			f.txByHash[h] = tx
		}
	}
	return f
}

// txHash derives a deterministic 32-byte hash from (blockNumber, index).
func txHash(bn uint64, idx int) string {
	var h common.Hash
	h[0] = byte(bn)
	h[1] = byte(idx)
	h[31] = 0xff
	return "0x" + hex.EncodeToString(h[:])
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
		s := strings.TrimPrefix(tag, "0x")
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

func (f *fakeRPC) handleRawTx(params []json.RawMessage) (any, *rpcErr) {
	if len(params) < 1 {
		return nil, &rpcErr{Code: -32602, Message: "missing hash"}
	}
	var h string
	if err := json.Unmarshal(params[0], &h); err != nil {
		return nil, &rpcErr{Code: -32602, Message: err.Error()}
	}
	tx, ok := f.txByHash[h]
	if !ok {
		return nil, &rpcErr{Code: -32602, Message: "unknown tx hash " + h}
	}
	return "0x" + hex.EncodeToString(tx), nil
}

// wireBlockFor renders a payload into the JSON shape eth_getBlockByNumber
// would return for the heal-payloads CLI.
func wireBlockFor(p *payloads.ExecutionPayloadV3) map[string]any {
	txHashes := make([]string, len(p.Transactions))
	for i := range p.Transactions {
		txHashes[i] = txHash(p.BlockNumber, i)
	}
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
		"number":        fmt.Sprintf("0x%x", p.BlockNumber),
		"hash":          "0x" + hex.EncodeToString(p.BlockHash[:]),
		"parentHash":    "0x" + hex.EncodeToString(p.ParentHash[:]),
		"miner":         "0x" + hex.EncodeToString(p.FeeRecipient[:]),
		"stateRoot":     "0x" + hex.EncodeToString(p.StateRoot[:]),
		"receiptsRoot":  "0x" + hex.EncodeToString(p.ReceiptsRoot[:]),
		"logsBloom":     "0x" + hex.EncodeToString(p.LogsBloom[:]),
		"mixHash":       "0x" + hex.EncodeToString(p.PrevRandao[:]),
		"gasLimit":      fmt.Sprintf("0x%x", p.GasLimit),
		"gasUsed":       fmt.Sprintf("0x%x", p.GasUsed),
		"timestamp":     fmt.Sprintf("0x%x", p.Timestamp),
		"extraData":     "0x" + hex.EncodeToString(p.ExtraData),
		"baseFeePerGas": fmt.Sprintf("0x%x", p.BaseFeePerGas),
		"blobGasUsed":   fmt.Sprintf("0x%x", p.BlobGasUsed),
		"excessBlobGas": fmt.Sprintf("0x%x", p.ExcessBlobGas),
		"transactions":  txHashes,
		"withdrawals":   withdrawals,
	}
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
		case "eth_getRawTransactionByHash":
			result, rerr = f.handleRawTx(req.Params)
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
		out = append(out, p.BlockNumber)
	}
}

func TestHealPayloadsFillsGap(t *testing.T) {
	dir := t.TempDir()
	inPath := filepath.Join(dir, "payloads.rlp")
	outPath := filepath.Join(dir, "payloads.rlp.healed")

	// Build 5 payloads, write only [1,2,5] to disk; serve 3 and 4 via the
	// fake RPC. Heal must produce a contiguous [1..5] output.
	all := make([]*payloads.ExecutionPayloadV3, 5)
	for i := range all {
		all[i] = fakePayload(uint64(i + 1))
	}
	writePayloadsFile(t, inPath, []*payloads.ExecutionPayloadV3{all[0], all[1], all[4]})

	fake := newFakeRPC(all, 5)
	srv := newFakeRPCServer(t, fake)

	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("rpc.NewClient: %v", err)
	}
	fetcher := newRPCBlockFetcher(client)

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

	all := make([]*payloads.ExecutionPayloadV3, 5)
	for i := range all {
		all[i] = fakePayload(uint64(i + 1))
	}
	// Input has [1,2,3]; node head is 5 → tail must add 4 and 5.
	writePayloadsFile(t, inPath, all[:3])
	fake := newFakeRPC(all, 5)
	srv := newFakeRPCServer(t, fake)
	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("rpc.NewClient: %v", err)
	}

	summary, err := Heal(context.Background(), HealConfig{
		InputPath:   inPath,
		OutputPath:  outPath,
		Fetcher:     newRPCBlockFetcher(client),
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
