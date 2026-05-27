package replay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	goethtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// ---- helpers ----------------------------------------------------------------

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b) //nolint:staticcheck
	return b
}

func randPayload(rng *rand.Rand, i int) *payloads.ExecutionPayloadV3 {
	var bloom [256]byte
	rng.Read(bloom[:]) //nolint:staticcheck

	var parentHash, stateRoot, receiptsRoot, prevRandao, blockHash [32]byte
	rng.Read(parentHash[:])  //nolint:staticcheck
	rng.Read(stateRoot[:])   //nolint:staticcheck
	rng.Read(receiptsRoot[:]) //nolint:staticcheck
	rng.Read(prevRandao[:])  //nolint:staticcheck
	rng.Read(blockHash[:])   //nolint:staticcheck

	var feeRecipient [20]byte
	rng.Read(feeRecipient[:]) //nolint:staticcheck

	return &payloads.ExecutionPayloadV3{
		ParentHash:    parentHash,
		FeeRecipient:  feeRecipient,
		StateRoot:     stateRoot,
		ReceiptsRoot:  receiptsRoot,
		LogsBloom:     bloom[:],
		Random:        prevRandao,
		Number:        uint64(i + 1),
		GasLimit:      30_000_000,
		GasUsed:       uint64(rng.Intn(30_000_000)),
		Timestamp:     uint64(1_700_000_000 + i*12),
		ExtraData:     randBytes(rng, rng.Intn(32)),
		BaseFeePerGas: new(big.Int).SetUint64(uint64(rng.Intn(1e9) + 1)),
		BlockHash:     blockHash,
		Transactions:  [][]byte{randBytes(rng, 50)},
		Withdrawals:   []*goethtypes.Withdrawal{},
		BlobGasUsed:   new(uint64),
		ExcessBlobGas: new(uint64),
	}
}

// writePayloadsFile writes ps to a temp file and returns its path.
func writePayloadsFile(t *testing.T, ps []*payloads.ExecutionPayloadV3) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payloads.rlp")
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
	return path
}

// blockHashHex returns the 0x-prefixed block hash for payload p.
func blockHashHex(p *payloads.ExecutionPayloadV3) string {
	var sb strings.Builder
	sb.WriteString("0x")
	for _, b := range p.BlockHash {
		sb.WriteString(string([]byte{
			hexNibble(b >> 4),
			hexNibble(b & 0x0f),
		}))
	}
	return sb.String()
}

func hexNibble(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + n - 10
}

// mockResponse encodes a single JSON-RPC result response.
func mockResponse(id json.RawMessage, result any) []byte {
	res, _ := json.Marshal(result)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  json.RawMessage(res),
	})
	return body
}

// rpcID extracts the "id" field from a JSON-RPC request body.
func rpcID(body []byte) json.RawMessage {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	json.Unmarshal(body, &req)
	return req.ID
}

// rpcMethod extracts the "method" field from a JSON-RPC request body.
func rpcMethod(body []byte) string {
	var req struct {
		Method string `json:"method"`
	}
	json.Unmarshal(body, &req)
	return req.Method
}

// newEngineServer creates an httptest.Server whose handler is driven by calls.
// Each call receives the raw request body and returns a response body.
type handlerFunc func(body []byte) []byte

func newEngineServer(t *testing.T, handler handlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp := handler(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// validHandler returns VALID for engine_newPayloadV4 and engine_forkchoiceUpdatedV3
// (echoing back the block hash from the payload param) and a configurable
// stateRoot for eth_getBlockByNumber.
func validHandler(stateRoot string) handlerFunc {
	return func(body []byte) []byte {
		id := rpcID(body)
		method := rpcMethod(body)
		switch method {
		case "engine_newPayloadV4":
			// Extract blockHash from the first param.
			var req struct {
				Params []json.RawMessage `json:"params"`
			}
			json.Unmarshal(body, &req)
			var wp struct{ BlockHash string `json:"blockHash"` }
			json.Unmarshal(req.Params[0], &wp)
			return mockResponse(id, map[string]any{
				"status":          "VALID",
				"latestValidHash": wp.BlockHash,
				"validationError": nil,
			})
		case "engine_forkchoiceUpdatedV3":
			return mockResponse(id, map[string]any{
				"payloadStatus": map[string]any{
					"status":          "VALID",
					"latestValidHash": "0x" + strings.Repeat("00", 32),
					"validationError": nil,
				},
				"payloadId": nil,
			})
		case "eth_getBlockByNumber":
			return mockResponse(id, map[string]any{
				"stateRoot": stateRoot,
			})
		default:
			return mockResponse(id, nil)
		}
	}
}

// ---- tests ------------------------------------------------------------------

// TestReplaySuccess: all payloads VALID, state root matches → nil error.
func TestReplaySuccess(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	ps := make([]*payloads.ExecutionPayloadV3, 3)
	for i := range ps {
		ps[i] = randPayload(rng, i)
	}

	// Use the last payload's stateRoot as the manifest's FinalStateRoot.
	wantRoot := "0x" + strings.ToLower(strings.TrimPrefix(
		blockHashHex(ps[len(ps)-1]), "0x"))
	// Actually, use a fixed expected root for simplicity.
	const expectedRoot = "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	srv := newEngineServer(t, validHandler(expectedRoot))
	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	path := writePayloadsFile(t, ps)
	d := &Driver{
		Client: client,
		Manifest: &manifest.Manifest{
			FinalStateRoot: expectedRoot,
		},
	}

	if err := d.Replay(context.Background(), path); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
	_ = wantRoot
}

// TestReplayInvalid: server returns INVALID for the second payload → ErrReplayInvalid.
func TestReplayInvalid(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	ps := make([]*payloads.ExecutionPayloadV3, 3)
	for i := range ps {
		ps[i] = randPayload(rng, i)
	}

	var callCount atomic.Int32
	handler := func(body []byte) []byte {
		id := rpcID(body)
		method := rpcMethod(body)
		switch method {
		case "engine_newPayloadV4":
			n := callCount.Add(1)
			var req struct {
				Params []json.RawMessage `json:"params"`
			}
			json.Unmarshal(body, &req)
			var wp struct{ BlockHash string `json:"blockHash"` }
			json.Unmarshal(req.Params[0], &wp)
			// Second newPayload call → INVALID.
			if n == 2 {
				return mockResponse(id, map[string]any{
					"status":          "INVALID",
					"latestValidHash": nil,
					"validationError": "bad block",
				})
			}
			return mockResponse(id, map[string]any{
				"status":          "VALID",
				"latestValidHash": wp.BlockHash,
				"validationError": nil,
			})
		case "engine_forkchoiceUpdatedV3":
			return mockResponse(id, map[string]any{
				"payloadStatus": map[string]any{
					"status":          "VALID",
					"latestValidHash": "0x" + strings.Repeat("00", 32),
					"validationError": nil,
				},
				"payloadId": nil,
			})
		default:
			return mockResponse(id, nil)
		}
	}

	srv := newEngineServer(t, handler)
	client, _ := rpc.NewClient(srv.URL)
	path := writePayloadsFile(t, ps)
	d := &Driver{
		Client:   client,
		Manifest: &manifest.Manifest{},
	}

	err := d.Replay(context.Background(), path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isReplayInvalid(err) {
		t.Fatalf("expected ErrReplayInvalid, got: %v", err)
	}
}

// TestReplayMismatch: all VALID but eth_getBlockByNumber returns wrong stateRoot → ErrReplayMismatch.
func TestReplayMismatch(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	ps := make([]*payloads.ExecutionPayloadV3, 3)
	for i := range ps {
		ps[i] = randPayload(rng, i)
	}

	const returnedRoot = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	srv := newEngineServer(t, validHandler(returnedRoot))
	client, _ := rpc.NewClient(srv.URL)
	path := writePayloadsFile(t, ps)
	d := &Driver{
		Client: client,
		Manifest: &manifest.Manifest{
			FinalStateRoot: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	err := d.Replay(context.Background(), path)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !isReplayMismatch(err) {
		t.Fatalf("expected ErrReplayMismatch, got: %v", err)
	}
}

// TestReplayPayloadCount: mock counts engine_newPayloadV4 calls == number of payloads.
func TestReplayPayloadCount(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	const n = 3
	ps := make([]*payloads.ExecutionPayloadV3, n)
	for i := range ps {
		ps[i] = randPayload(rng, i)
	}

	const expectedRoot = "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	var newPayloadCalls atomic.Int32

	handler := func(body []byte) []byte {
		id := rpcID(body)
		method := rpcMethod(body)
		switch method {
		case "engine_newPayloadV4":
			newPayloadCalls.Add(1)
			var req struct {
				Params []json.RawMessage `json:"params"`
			}
			json.Unmarshal(body, &req)
			var wp struct{ BlockHash string `json:"blockHash"` }
			json.Unmarshal(req.Params[0], &wp)
			return mockResponse(id, map[string]any{
				"status":          "VALID",
				"latestValidHash": wp.BlockHash,
				"validationError": nil,
			})
		case "engine_forkchoiceUpdatedV3":
			return mockResponse(id, map[string]any{
				"payloadStatus": map[string]any{
					"status":          "VALID",
					"latestValidHash": "0x" + strings.Repeat("00", 32),
					"validationError": nil,
				},
				"payloadId": nil,
			})
		case "eth_getBlockByNumber":
			return mockResponse(id, map[string]any{"stateRoot": expectedRoot})
		default:
			return mockResponse(id, nil)
		}
	}

	srv := newEngineServer(t, handler)
	client, _ := rpc.NewClient(srv.URL)
	path := writePayloadsFile(t, ps)
	d := &Driver{
		Client: client,
		Manifest: &manifest.Manifest{
			FinalStateRoot: expectedRoot,
		},
	}

	if err := d.Replay(context.Background(), path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := int(newPayloadCalls.Load()); got != n {
		t.Fatalf("engine_newPayloadV4 called %d times, want %d", got, n)
	}
}

// TestReplayNoManifestRoot: no FinalStateRoot in manifest → skip state root check.
func TestReplayNoManifestRoot(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	ps := []*payloads.ExecutionPayloadV3{randPayload(rng, 0)}

	srv := newEngineServer(t, validHandler("0x" + strings.Repeat("ff", 32)))
	client, _ := rpc.NewClient(srv.URL)
	path := writePayloadsFile(t, ps)
	d := &Driver{
		Client:   client,
		Manifest: &manifest.Manifest{}, // empty FinalStateRoot
	}

	if err := d.Replay(context.Background(), path); err != nil {
		t.Fatalf("expected nil with no manifest root, got: %v", err)
	}
}

// --- sentinel helpers --------------------------------------------------------

func isReplayInvalid(err error) bool {
	return errors.Is(err, ErrReplayInvalid)
}

func isReplayMismatch(err error) bool {
	return errors.Is(err, ErrReplayMismatch)
}
