package sensor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

// makeStatecompResponse builds the JSON body that statecomp_get returns.
func makeStatecompResponse(blockNumber uint64) map[string]any {
	return map[string]any{
		"blockNumber": fmt.Sprintf("0x%x", blockNumber),
		"trieStats": map[string]any{
			"accountTrieBytes": fmt.Sprintf("0x%x", blockNumber*1000),
			"storageTrieBytes": fmt.Sprintf("0x%x", blockNumber*2000),
			"codeBytesTotal":   fmt.Sprintf("0x%x", blockNumber*500),
		},
	}
}

// newStatecompServer creates an httptest.Server that responds to statecomp_get.
// The blockFn callback is called on each request to determine the current block.
func newStatecompServer(t *testing.T, blockFn func() uint64) (*httptest.Server, *rpc.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		bn := blockFn()
		result := makeStatecompResponse(bn)
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}))
	t.Cleanup(srv.Close)

	c, err := rpc.NewClient(srv.URL, rpc.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return srv, c
}

// TestReadReturnsWhenBlockReached verifies that Read returns as soon as
// blockNumber reaches the expected value.
func TestReadReturnsWhenBlockReached(t *testing.T) {
	var counter atomic.Uint64
	counter.Store(0)

	_, c := newStatecompServer(t, func() uint64 { return counter.Load() })

	// Increment the counter in a background goroutine to simulate block progress.
	go func() {
		for i := uint64(1); i <= 6; i++ {
			time.Sleep(60 * time.Millisecond)
			counter.Store(i)
		}
	}()

	s := sensor.New(c,
		sensor.WithPollInterval(30*time.Millisecond),
		sensor.WithDeadline(3*time.Second),
	)

	snap, err := s.Read(context.Background(), 5)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.BlockNumber < 5 {
		t.Errorf("expected blockNumber >= 5, got %d", snap.BlockNumber)
	}
	if snap.Raw == nil {
		t.Error("Raw is nil")
	}
	if snap.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero")
	}
}

// TestReadTimeoutReturnsSensorTimeout verifies that a stale server triggers
// ErrSensorTimeout and that the last snapshot is non-nil.
func TestReadTimeoutReturnsSensorTimeout(t *testing.T) {
	// Server always returns blockNumber=1.
	_, c := newStatecompServer(t, func() uint64 { return 1 })

	s := sensor.New(c,
		sensor.WithPollInterval(20*time.Millisecond),
		sensor.WithDeadline(200*time.Millisecond),
	)

	snap, err := s.Read(context.Background(), 999)
	if !errors.Is(err, sensor.ErrSensorTimeout) {
		t.Fatalf("expected ErrSensorTimeout, got %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil last snapshot on timeout, got nil")
	}
	if snap.BlockNumber != 1 {
		t.Errorf("expected last block 1, got %d", snap.BlockNumber)
	}
}

// TestReadCancelledContextReturnsPromptly verifies that cancelling the context
// causes Read to return well within 200ms.
func TestReadCancelledContextReturnsPromptly(t *testing.T) {
	// Server responds very slowly to verify we don't wait for pending calls.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	c, err := rpc.NewClient(srv.URL, rpc.WithTimeout(1*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	s := sensor.New(c,
		sensor.WithPollInterval(10*time.Millisecond),
		sensor.WithDeadline(5*time.Second),
	)

	ctx, cancel := context.WithCancel(context.Background())

	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err = s.Read(ctx, 999)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("Read took %v after cancel, expected < 200ms", elapsed)
	}
}

// TestSnapshotFields verifies that trie byte fields are correctly parsed.
func TestSnapshotFields(t *testing.T) {
	const bn = uint64(42)
	_, c := newStatecompServer(t, func() uint64 { return bn })

	s := sensor.New(c,
		sensor.WithPollInterval(10*time.Millisecond),
		sensor.WithDeadline(2*time.Second),
	)

	snap, err := s.Read(context.Background(), bn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snap.AccountTrieBytes != bn*1000 {
		t.Errorf("AccountTrieBytes: got %d, want %d", snap.AccountTrieBytes, bn*1000)
	}
	if snap.StorageTrieBytes != bn*2000 {
		t.Errorf("StorageTrieBytes: got %d, want %d", snap.StorageTrieBytes, bn*2000)
	}
	if snap.CodeBytesTotal != bn*500 {
		t.Errorf("CodeBytesTotal: got %d, want %d", snap.CodeBytesTotal, bn*500)
	}
}
