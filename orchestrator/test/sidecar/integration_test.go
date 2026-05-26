// Package sidecartest contains end-to-end integration tests that exercise the
// sidecar's modules together: tracker, snapshot, RPC, and the in-memory
// equivalent of the tailer path (decode-RLP → apply to tracker).
//
// These tests deliberately avoid RocksDB so they run on the developer's
// machine without CGO. The tailer's tick() function reads from the FlatDb
// using grocksdb iterators; here we feed records to tracker.ApplyBlockDiff
// directly, which is what tailer.tick() also does after decoding.
package sidecartest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	rlppkg "github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rlp"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/snapshot"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tailer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

func TestBootstrapTailServeFlow(t *testing.T) {
	// 1. Simulate a bootstrap result by seeding a tracker with scan-only data
	//    and per-key seeds.
	tr := tracker.New()
	tr.SetScanCounters(tracker.ScanCounters{
		BlockNumber:           1000,
		StateRoot:             [32]byte{0x78, 0x45, 0xcf, 0x57},
		AccountsTotal:         500_000,
		EmptyAccounts:         42,
		AccountTrieBranches:   1234,
		AccountTrieExtensions: 567,
		AccountTrieLeaves:     500_000,
		AccountTrieBytes:      10_000_000,
		StorageTrieBranches:   2222,
		StorageTrieExtensions: 1111,
		StorageTrieLeaves:     1_000_000,
		StorageTrieBytes:      20_000_000,
		SlotHistogram:         []int64{0, 100, 200, 300, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	})
	tr.SeedFromScan(
		[]tracker.CodeSeed{
			{CodeHash: [32]byte{0xa1}, CodeSize: 1024, Refcount: 50},
			{CodeHash: [32]byte{0xa2}, CodeSize: 2048, Refcount: 100},
		},
		[]tracker.SlotSeed{
			{HashedAddress: [32]byte{0xb1}, SlotCount: 1000},
			{HashedAddress: [32]byte{0xb2}, SlotCount: 50},
		},
	)
	tr.SetLastBlock(1000, [32]byte{0x78, 0x45, 0xcf, 0x57})

	// 2. Persist to a snapshot.
	dir := t.TempDir()
	if _, err := snapshot.Write(dir, tr); err != nil {
		t.Fatalf("snapshot write: %v", err)
	}

	// 3. Restore in a fresh process simulation.
	tr2, hdr, err := snapshot.Restore(dir)
	if err != nil {
		t.Fatalf("snapshot restore: %v", err)
	}
	if hdr.BlockNumber != 1000 {
		t.Fatalf("restored hdr.BlockNumber = %d, want 1000", hdr.BlockNumber)
	}

	// 4. Apply a synthetic block diff that mimics what the Phase 2 plugin
	//    will write into the BlockDiffs CF.
	diff := rlppkg.BlockDiffRecord{
		BlockNumber: 1001,
		StateRoot:   [32]byte{0x99},
		CodeHashChanges: []rlppkg.CodeHashChange{
			{OldHash: [32]byte{}, NewHash: [32]byte{0xa3}, NewCodeSize: 4096},
		},
		SlotCountChanges: []rlppkg.SlotCountChange{
			{HashedAddress: [32]byte{0xb1}, OldCount: 1000, NewCount: 1100},
		},
	}

	// 4a. Round-trip the diff through the canonical RLP wire format — this is
	//     exactly what the tailer does on each iteration.
	encoded, err := rlppkg.EncodeBlockDiff(diff)
	if err != nil {
		t.Fatalf("encode diff: %v", err)
	}
	decoded, err := rlppkg.DecodeBlockDiff(encoded)
	if err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	tr2.ApplyBlockDiff(decoded)

	// 5. Spin up the RPC server and query statecomp_lite + statecomp_get.
	s := rpc.New(tr2, "127.0.0.1:0", zerolog.Nop())
	s.NoteBootstrapDone()
	s.SetChainHead(1010)

	gotLite := callRPC(t, s, "statecomp_lite")
	if got := int64(gotLite["accountsTotal"].(float64)); got != 500_000 {
		t.Errorf("accountsTotal = %d, want 500_000", got)
	}
	// Original contractsTotal = sum(refcounts) = 150; +1 from the new code hash → 151.
	if got := int64(gotLite["contractsTotal"].(float64)); got != 151 {
		t.Errorf("contractsTotal = %d, want 151", got)
	}
	// codeBytesTotal is deduped by hash (no refcount weighting):
	// 1024 + 2048 + 4096 = 7168.
	if got := int64(gotLite["codeBytesTotal"].(float64)); got != 7168 {
		t.Errorf("codeBytesTotal = %d, want 7168", got)
	}
	// 1000 (b1 seed) + 50 (b2 seed) + 100 (b1 delta) = 1150.
	if got := int64(gotLite["storageSlotsTotal"].(float64)); got != 1150 {
		t.Errorf("storageSlotsTotal = %d, want 1150", got)
	}
	if got := int64(gotLite["incrementalBlockNumber"].(float64)); got != 1001 {
		t.Errorf("incrementalBlockNumber = %d, want 1001", got)
	}
	if got := int64(gotLite["blocksBehind"].(float64)); got != 9 {
		t.Errorf("blocksBehind = %d, want 9", got)
	}

	gotFull := callRPC(t, s, "statecomp_get")
	if got := int64(gotFull["storageTrieBytes"].(float64)); got != 20_000_000 {
		t.Errorf("storageTrieBytes = %d, want 20M", got)
	}
	hist := gotFull["slotCountHistogram"].([]any)
	if len(hist) != 16 {
		t.Fatalf("histogram len = %d, want 16", len(hist))
	}

	// Print the sample for the deliverable summary.
	pretty, _ := json.MarshalIndent(gotLite, "", "  ")
	t.Logf("statecomp_lite sample response:\n%s", pretty)
}

func TestTailerKeyEncoding(t *testing.T) {
	// Ensure tailer.EncodeKey produces 8 big-endian bytes — the canonical
	// schema the Phase 2 NM plugin must write.
	key := tailer.EncodeKey(12_345_678)
	want := make([]byte, 8)
	binary.BigEndian.PutUint64(want, 12_345_678)
	if !bytes.Equal(key, want) {
		t.Fatalf("EncodeKey mismatch: %x vs %x", key, want)
	}
}

func TestRPCMethodNotFound(t *testing.T) {
	tr := tracker.New()
	s := rpc.New(tr, "127.0.0.1:0", zerolog.Nop())
	got := callRawRPC(t, s, "unknown_method")
	if got["error"] == nil {
		t.Fatalf("expected error for unknown method, got %v", got)
	}
}

func callRPC(t *testing.T, s *rpc.Server, method string) map[string]any {
	t.Helper()
	resp := callRawRPC(t, s, method)
	if resp["error"] != nil {
		t.Fatalf("rpc error for %q: %v", method, resp["error"])
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %v", resp)
	}
	return res
}

func callRawRPC(t *testing.T, s *rpc.Server, method string) map[string]any {
	t.Helper()
	ts := httptest.NewServer(s)
	defer ts.Close()
	body := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`)
	resp, err := http.Post(ts.URL+"/", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}
