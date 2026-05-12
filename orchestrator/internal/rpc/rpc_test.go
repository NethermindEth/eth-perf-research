package rpc_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/golang-jwt/jwt/v5"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// jwtSecret is a test-only 32-byte key used in JWT tests.
var jwtSecret = strings.Repeat("a", 64) // 32 bytes hex-encoded

// newTestServer creates an httptest server that dispatches JSON-RPC methods via
// the provided handler map. Any method not in the map returns an internal error.
func newTestServer(t *testing.T, handlers map[string]func() any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fn, ok := handlers[req.Method]
		if !ok {
			enc := json.NewEncoder(w)
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}
		result := fn()
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChainID(t *testing.T) {
	srv := newTestServer(t, map[string]func() any{
		"eth_chainId": func() any { return "0x539" },
	})

	c, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	id, err := c.ChainID(context.Background())
	if err != nil {
		t.Fatalf("ChainID: %v", err)
	}
	if id != 1337 {
		t.Fatalf("expected 1337, got %d", id)
	}
}

func TestBlockByNumber(t *testing.T) {
	block := map[string]any{
		"number":        "0x10d4f",
		"hash":          "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"parentHash":    "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"stateRoot":     "0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"gasLimit":      "0x1c9c380",
		"gasUsed":       "0x5208",
		"timestamp":     "0x6612abcd",
		"baseFeePerGas": "0x3b9aca00",
		"transactions":  []string{},
	}

	srv := newTestServer(t, map[string]func() any{
		"eth_getBlockByNumber": func() any { return block },
	})

	c, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	hdr, err := c.BlockByNumber(context.Background(), -1) // "latest"
	if err != nil {
		t.Fatalf("BlockByNumber: %v", err)
	}
	if hdr.Number != 0x10d4f {
		t.Errorf("Number: got %d, want %d", hdr.Number, 0x10d4f)
	}
	if hdr.BaseFee == nil {
		t.Fatal("BaseFee is nil")
	}
	if hdr.BaseFee.Int64() != 1_000_000_000 {
		t.Errorf("BaseFee: got %s, want 1000000000", hdr.BaseFee)
	}
}

func TestTransactionCount(t *testing.T) {
	srv := newTestServer(t, map[string]func() any{
		"eth_getTransactionCount": func() any { return "0x42" },
	})

	c, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	count, err := c.TransactionCount(context.Background(), common.Address{})
	if err != nil {
		t.Fatalf("TransactionCount: %v", err)
	}
	if count != 0x42 {
		t.Errorf("expected 0x42=66, got %d", count)
	}
}

func TestTestingCommitBlockV1(t *testing.T) {
	wantHash := "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	srv := newTestServer(t, map[string]func() any{
		"testing_commitBlockV1": func() any { return wantHash },
	})

	c, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	hash, err := c.TestingCommitBlockV1(context.Background(), [][]byte{{0x01, 0x02}}, 0x6612abcd)
	if err != nil {
		t.Fatalf("TestingCommitBlockV1: %v", err)
	}
	if hash != common.HexToHash(wantHash) {
		t.Errorf("hash mismatch: got %s, want %s", hash.Hex(), wantHash)
	}
}

func TestRpcError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"oops"}}`)
	}))
	t.Cleanup(srv.Close)

	c, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var out any
	err = c.Call(context.Background(), "eth_chainId", nil, &out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *rpc.RpcError
	if !errorAs(err, &rpcErr) {
		t.Fatalf("expected *rpc.RpcError, got %T: %v", err, err)
	}
	if rpcErr.Code != -32600 {
		t.Errorf("Code: got %d, want -32600", rpcErr.Code)
	}
	if rpcErr.Message != "oops" {
		t.Errorf("Message: got %q, want %q", rpcErr.Message, "oops")
	}
}

// errorAs is a local type-assertion helper because errors.As requires a pointer
// to the target type.
func errorAs(err error, target **rpc.RpcError) bool {
	type asser interface {
		As(any) bool
	}
	// Use the standard library errors.As via unwrapping.
	for err != nil {
		if e, ok := err.(*rpc.RpcError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return false
}

func TestJWTRoundTripper(t *testing.T) {
	secretBytes := make([]byte, 32)
	for i := range secretBytes {
		secretBytes[i] = 0xaa
	}
	secretHex := fmt.Sprintf("%x", secretBytes) // 64 hex chars

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing Bearer", http.StatusUnauthorized)
			return
		}
		tokenStr := strings.TrimPrefix(auth, "Bearer ")
		tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return secretBytes, nil
		})
		if err != nil || !tok.Valid {
			http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":1,"result":"0x539"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := rpc.NewClient(srv.URL, rpc.WithJWTSecret(secretHex))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	id, err := c.ChainID(context.Background())
	if err != nil {
		t.Fatalf("ChainID with JWT: %v", err)
	}
	if id != 1337 {
		t.Errorf("expected 1337, got %d", id)
	}
}

func TestWithTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	t.Cleanup(srv.Close)

	c, err := rpc.NewClient(srv.URL, rpc.WithTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.ChainID(context.Background())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
