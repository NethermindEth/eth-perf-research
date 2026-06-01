package rpc

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

func newTestTracker(t *testing.T) *tracker.Tracker {
	tr := tracker.New()
	tr.SetScanCounters(tracker.ScanCounters{
		BlockNumber:           42,
		StateRoot:             [32]byte{0x01, 0x02, 0x03},
		AccountsTotal:         100,
		EmptyAccounts:         5,
		AccountTrieBranches:   10,
		AccountTrieExtensions: 11,
		AccountTrieLeaves:     12,
		AccountTrieBytes:      13,
		StorageTrieBranches:   14,
		StorageTrieExtensions: 15,
		StorageTrieLeaves:     16,
		StorageTrieBytes:      17,
		SlotHistogram:         []int64{0, 1, 2, 3, 4, 5, 6, 7, 0, 0, 0, 0, 0, 0, 0, 0},
	})
	tr.SeedFromScan(
		[]tracker.CodeSeed{{CodeHash: [32]byte{0xaa}, CodeSize: 500, Refcount: 3}},
		[]tracker.SlotSeed{{HashedAddress: [32]byte{0xbb}, SlotCount: 25}},
	)
	tr.SetLastBlock(42, [32]byte{0x01, 0x02, 0x03})
	return tr
}

func TestStatecompLiteSchema(t *testing.T) {
	tr := newTestTracker(t)
	s := New(tr, "127.0.0.1:0", zerolog.Nop())
	s.NoteBootstrapDone()
	s.SetChainHead(50)

	resp := postRPC(t, s, `{"jsonrpc":"2.0","id":1,"method":"statecomp_lite"}`)
	if resp["error"] != nil {
		t.Fatalf("rpc error: %v", resp["error"])
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %v", resp)
	}
	required := []string{
		"chainHeadBlockNumber", "incrementalBlockNumber", "blocksBehind",
		"diffsSinceBaseline", "hasIncrementalBaseline", "incrementalEnabled",
		"incrementalDetailEnabled", "accountsTotal", "contractsTotal",
		"storageSlotsTotal", "accountTrieBytes", "storageTrieBytes",
		"codeBytesTotal", "exactCountersActive", "exactCountersBlockNumber",
	}
	for _, k := range required {
		if _, ok := res[k]; !ok {
			t.Errorf("missing key %q in statecomp_lite response", k)
		}
	}
	if v := res["incrementalBlockNumber"]; v == nil {
		t.Errorf("incrementalBlockNumber nil")
	}
	if v := res["accountsTotal"].(float64); int64(v) != 100 {
		t.Errorf("accountsTotal = %v, want 100", v)
	}
	if v := res["contractsTotal"].(float64); int64(v) != 3 {
		t.Errorf("contractsTotal = %v, want 3", v)
	}
	if v := res["codeBytesTotal"].(float64); int64(v) != 500 {
		t.Errorf("codeBytesTotal = %v, want 500", v)
	}
	if v := res["blocksBehind"].(float64); int64(v) != 8 {
		t.Errorf("blocksBehind = %v, want 8", v)
	}
}

func TestStatecompGetIncludesHistogram(t *testing.T) {
	tr := newTestTracker(t)
	s := New(tr, "127.0.0.1:0", zerolog.Nop())
	resp := postRPC(t, s, `{"jsonrpc":"2.0","id":2,"method":"statecomp_get"}`)
	if resp["error"] != nil {
		t.Fatalf("rpc error: %v", resp["error"])
	}
	res := resp["result"].(map[string]any)
	hist, ok := res["slotCountHistogram"].([]any)
	if !ok {
		t.Fatalf("slotCountHistogram missing / wrong type: %v", res["slotCountHistogram"])
	}
	if len(hist) != 16 {
		t.Fatalf("histogram len = %d, want 16", len(hist))
	}
	if int64(hist[7].(float64)) != 7 {
		t.Errorf("hist[7] = %v, want 7", hist[7])
	}
}

// TestStatecompGetMatchesLegacyNMShape pins the wire schema: nested `trieStats`
// plus top-level `blockNumber`, matching the C# StateCompositionReport shape.
// Divergence causes sensor.go's parseSnapshot to silently return zeros.
func TestStatecompGetMatchesLegacyNMShape(t *testing.T) {
	tr := newTestTracker(t)
	s := New(tr, "127.0.0.1:0", zerolog.Nop())
	resp := postRPC(t, s, `{"jsonrpc":"2.0","id":7,"method":"statecomp_get"}`)
	if resp["error"] != nil {
		t.Fatalf("rpc error: %v", resp["error"])
	}
	res := resp["result"].(map[string]any)

	if _, ok := res["blockNumber"]; !ok {
		t.Fatalf("missing top-level blockNumber: %v", res)
	}
	if int64(res["blockNumber"].(float64)) != 42 {
		t.Errorf("blockNumber = %v, want 42", res["blockNumber"])
	}

	trieStats, ok := res["trieStats"].(map[string]any)
	if !ok {
		t.Fatalf("missing nested trieStats object: %v", res["trieStats"])
	}
	for _, k := range []string{"accountTrieBytes", "storageTrieBytes", "codeBytesTotal"} {
		if _, ok := trieStats[k]; !ok {
			t.Errorf("missing trieStats.%s", k)
		}
	}
	if int64(trieStats["accountTrieBytes"].(float64)) != 13 {
		t.Errorf("trieStats.accountTrieBytes = %v, want 13", trieStats["accountTrieBytes"])
	}
	if int64(trieStats["storageTrieBytes"].(float64)) != 17 {
		t.Errorf("trieStats.storageTrieBytes = %v, want 17", trieStats["storageTrieBytes"])
	}
	if int64(trieStats["codeBytesTotal"].(float64)) != 500 {
		t.Errorf("trieStats.codeBytesTotal = %v, want 500", trieStats["codeBytesTotal"])
	}

	for _, k := range []string{
		"accountsTotal", "contractsTotal", "storageSlotsTotal",
		"accountTrieBranches", "accountTrieExtensions", "accountTrieLeaves",
		"storageTrieBranches", "storageTrieExtensions", "storageTrieLeaves",
		"contractsWithStorage", "emptyAccounts", "slotCountHistogram",
	} {
		if _, ok := trieStats[k]; !ok {
			t.Errorf("missing trieStats.%s — diverges from CumulativeTrieStats", k)
		}
	}
}

func TestStatecompHealthLagAndUptime(t *testing.T) {
	tr := newTestTracker(t)
	s := New(tr, "127.0.0.1:0", zerolog.Nop())
	s.NoteBootstrapDone()
	s.SetChainHead(45)
	resp := postRPC(t, s, `{"jsonrpc":"2.0","id":3,"method":"statecomp_health"}`)
	res := resp["result"].(map[string]any)
	if res["bootstrapCompleted"] != true {
		t.Errorf("bootstrapCompleted = %v, want true", res["bootstrapCompleted"])
	}
	if int64(res["lagBlocks"].(float64)) != 3 {
		t.Errorf("lagBlocks = %v, want 3", res["lagBlocks"])
	}
}

func TestServerEndToEndOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	tr := newTestTracker(t)
	s := New(tr, ln.Addr().String(), zerolog.Nop())
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"statecomp_lite"}`)
	resp, err := http.Post("http://"+ln.Addr().String()+"/", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["result"] == nil {
		t.Fatalf("nil result: %v", got)
	}
}

func postRPC(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.handle(w, req)
	resp := w.Result()
	defer resp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}
