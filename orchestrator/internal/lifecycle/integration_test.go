//go:build integration

package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
)

// hardhat default deploy key (well-known test key, not secret).
const deployKey = "0x4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

// genesisHash is a deterministic 32-byte hex used as the genesis SHA256.
const genesisHash = "0000000000000000000000000000000000000000000000000000000000000001"

// targetYAML is the minimal target config written into the temp state dir.
const targetYAML = `shares:
  accounts: 0.33
  storage: 0.34
  code: 0.33
total_bytes: 1073741824
`

func TestLifecycleIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	workerBin := buildLifecycleWorker(t)

	// ── mock Nethermind ──────────────────────────────────────────────────────
	nm := newMockNM(t)
	defer nm.srv.Close()

	// ── state directory ──────────────────────────────────────────────────────
	stateDir := t.TempDir()

	targetPath := filepath.Join(stateDir, "target.yaml")
	if err := os.WriteFile(targetPath, []byte(targetYAML), 0o644); err != nil {
		t.Fatalf("write target.yaml: %v", err)
	}

	// ── run ──────────────────────────────────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := Config{
		RPCURL:             nm.srv.URL,
		StateDir:           stateDir,
		TargetYAMLPath:     targetPath,
		GenesisSHA256:      genesisHash,
		MaxBatches:         5,
		EnableProbe:        false,
		DeployPrivateKey:   deployKey,
		BuilderWorkerCmd:   []string{workerBin},
		BuilderWorkers:     1,
		SensorPollInterval: 10 * time.Millisecond,
		SensorDeadline:     3 * time.Second,
		Verbs:              []string{"eoatx"},
	}

	if err := Run(ctx, cfg); err != nil && ctx.Err() == nil {
		t.Fatalf("lifecycle.Run: %v", err)
	}

	// ── assertions ───────────────────────────────────────────────────────────
	journalPath := filepath.Join(stateDir, "journal.bin")
	payloadsPath := filepath.Join(stateDir, "payloads.rlp")
	manifestPath := filepath.Join(stateDir, "run-manifest.json")

	// 1. journal.bin exists with 5 records, chain-hash verifies
	assertJournal(t, journalPath, 5)

	// 2. payloads.rlp exists with 5 entries
	assertPayloads(t, payloadsPath, 5)

	// 3. run-manifest.json has BatchCount=5, Terminated="batch_limit"
	assertManifest(t, manifestPath, 5, "batch_limit")
}

// ── mock NM ──────────────────────────────────────────────────────────────────

type mockNM struct {
	srv          *httptest.Server
	mu           sync.Mutex
	blockNumber  uint64            // increments each testing_commitBlockV1 call
	statecompCalls uint64          // atomic counter for growing trie sizes
}

func newMockNM(t *testing.T) *mockNM {
	t.Helper()
	m := &mockNM{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

func (m *mockNM) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse json", http.StatusBadRequest)
		return
	}

	var result any
	switch req.Method {
	case "eth_chainId":
		result = "0x539" // 1337

	case "eth_getBlockByNumber":
		m.mu.Lock()
		bn := m.blockNumber
		m.mu.Unlock()
		result = m.blockJSON(bn)

	case "eth_getBlockByHash":
		// params: [hash, full]
		var params []json.RawMessage
		_ = json.Unmarshal(req.Params, &params)
		var hashStr string
		if len(params) > 0 {
			_ = json.Unmarshal(params[0], &hashStr)
		}
		m.mu.Lock()
		bn := m.blockNumber
		m.mu.Unlock()
		result = m.blockWithHashJSON(hashStr, bn)

	case "eth_getTransactionCount":
		result = "0x0"

	case "testing_commitBlockV1":
		// Deterministic block hash: sha256 of the request body, hex-encoded.
		h := sha256.Sum256(body)
		blockHash := "0x" + hex.EncodeToString(h[:])
		m.mu.Lock()
		m.blockNumber++
		m.mu.Unlock()
		result = blockHash

	case "statecomp_get":
		calls := atomic.AddUint64(&m.statecompCalls, 1)
		growBy := calls * 1_000_000 // ~1 MB per call
		m.mu.Lock()
		bn := m.blockNumber
		m.mu.Unlock()
		result = map[string]any{
			"blockNumber": fmt.Sprintf("0x%x", bn),
			"trieStats": map[string]any{
				"accountTrieBytes": growBy,
				"storageTrieBytes": growBy,
				"codeBytesTotal":   growBy,
			},
		}

	default:
		result = nil
	}

	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(resp)
}

func (m *mockNM) blockJSON(bn uint64) map[string]any {
	return map[string]any{
		"number":       fmt.Sprintf("0x%x", bn),
		"hash":         blockHashForNumber(bn),
		"parentHash":   "0x" + strings.Repeat("00", 32),
		"stateRoot":    "0xabc0000000000000000000000000000000000000000000000000000000000123",
		"baseFeePerGas": "0x3b9aca00",
		"gasLimit":     "0xee6b2800",
		"gasUsed":      "0x0",
		"timestamp":    "0x671d3f00",
		"transactions": []any{},
	}
}

func (m *mockNM) blockWithHashJSON(hash string, bn uint64) map[string]any {
	blk := m.blockJSON(bn)
	if hash != "" {
		blk["hash"] = hash
	}
	return blk
}

func blockHashForNumber(n uint64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("block-%d", n)))
	return "0x" + hex.EncodeToString(h[:])
}

// ── lifecycle_worker build helper ────────────────────────────────────────────

func buildLifecycleWorker(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = .../orchestrator/internal/lifecycle/integration_test.go
	moduleRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	srcPkg := filepath.Join(moduleRoot, "testdata", "lifecycle_worker")

	bin := filepath.Join(t.TempDir(), "lifecycle_worker")
	cmd := exec.Command("go", "build", "-o", bin, srcPkg)
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build lifecycle_worker: %v", err)
	}
	return bin
}

// ── assertion helpers ────────────────────────────────────────────────────────

func assertJournal(t *testing.T, path string, wantRecords int) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal.bin not found: %v", err)
	}
	count, _, err := journal.VerifyAll(path)
	if err != nil {
		t.Fatalf("journal.VerifyAll: %v", err)
	}
	if count != wantRecords {
		t.Errorf("journal record count = %d, want %d", count, wantRecords)
	}
}

func assertPayloads(t *testing.T, path string, wantEntries int) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("payloads.rlp not found: %v", err)
	}
	pr, err := payloads.OpenReader(path)
	if err != nil {
		t.Fatalf("payloads.OpenReader: %v", err)
	}
	defer pr.Close()

	count := 0
	for {
		_, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("payloads.Next[%d]: %v", count, err)
		}
		count++
	}
	if count != wantEntries {
		t.Errorf("payloads entry count = %d, want %d", count, wantEntries)
	}
}

func assertManifest(t *testing.T, path string, wantBatchCount int, wantTerminated string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("run-manifest.json not found: %v", err)
	}
	mf, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("manifest.Load: %v", err)
	}
	if mf.BatchCount != wantBatchCount {
		t.Errorf("manifest.BatchCount = %d, want %d", mf.BatchCount, wantBatchCount)
	}
	if mf.Terminated != wantTerminated {
		t.Errorf("manifest.Terminated = %q, want %q", mf.Terminated, wantTerminated)
	}
}
