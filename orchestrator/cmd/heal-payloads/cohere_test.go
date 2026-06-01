package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// orphanPayload is a frame for block n that does NOT link to the canonical
// chain: its parent and self hashes are off-chain sentinels.
func orphanPayload(n uint64) *payloads.ExecutionPayloadV3 {
	p := fakePayload(n)
	var off common.Hash
	for i := range off {
		off[i] = 0xee
	}
	p.ParentHash = off
	p.BlockHash = off
	return p
}

func cohereFetcher(t *testing.T, canonical []*payloads.ExecutionPayloadV3, head uint64) HashFetcher {
	t.Helper()
	fake := newFakeRPC(canonical, head)
	srv := newFakeRPCServer(t, fake)
	client, err := rpc.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("rpc.NewClient: %v", err)
	}
	return newRPCBlockFetcher(client)
}

// canonicalBlocks returns fakePayload(0..n] inclusive of 0 so BlockHashAt(from-1)
// can be served for from=1.
func canonicalBlocks(n uint64) []*payloads.ExecutionPayloadV3 {
	out := make([]*payloads.ExecutionPayloadV3, n+1)
	for i := uint64(0); i <= n; i++ {
		out[i] = fakePayload(i)
	}
	return out
}

func TestCohereHeadAndInternalGap(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "payloads.rlp")
	out := filepath.Join(dir, "payloads.rlp.cohered")

	all := canonicalBlocks(5)
	// Input starts at 3 (missing head 1,2) and omits 4 (internal gap): [3,5].
	writePayloadsFile(t, in, []*payloads.ExecutionPayloadV3{all[3], all[5]})

	summary, err := Cohere(context.Background(), CohereConfig{
		InputPath: in, OutputPath: out, From: 1, Head: 5,
		Fetcher: cohereFetcher(t, all, 5),
	})
	if err != nil {
		t.Fatalf("Cohere: %v", err)
	}
	if got := readPayloadNumbers(t, out); !equalU64(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("blocks: got %v want [1 2 3 4 5]", got)
	}
	if summary.Fetched != 3 { // 1,2 (head) + 4 (gap)
		t.Errorf("Fetched: got %d want 3", summary.Fetched)
	}
	if summary.Reused != 2 { // 3,5
		t.Errorf("Reused: got %d want 2", summary.Reused)
	}
}

func TestCohereReorgLastOccurrenceWins(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "payloads.rlp")
	out := filepath.Join(dir, "payloads.rlp.cohered")

	all := canonicalBlocks(5)
	// Simulate a reorg recorded in the file: committed 1,2,3,4 then rolled back
	// and re-committed 3,4,5. Last occurrence of 3 and 4 is canonical.
	writePayloadsFile(t, in, []*payloads.ExecutionPayloadV3{
		all[1], all[2], all[3], all[4], all[3], all[4], all[5],
	})

	summary, err := Cohere(context.Background(), CohereConfig{
		InputPath: in, OutputPath: out, From: 1, Head: 5,
		Fetcher: cohereFetcher(t, all, 5),
	})
	if err != nil {
		t.Fatalf("Cohere: %v", err)
	}
	if got := readPayloadNumbers(t, out); !equalU64(got, []uint64{1, 2, 3, 4, 5}) {
		t.Fatalf("blocks: got %v want [1 2 3 4 5]", got)
	}
	if summary.Fetched != 0 || summary.Relinked != 0 {
		t.Errorf("expected all reused: fetched=%d relinked=%d", summary.Fetched, summary.Relinked)
	}
	if summary.Reused != 5 {
		t.Errorf("Reused: got %d want 5", summary.Reused)
	}
}

func TestCohereRelinkOrphan(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "payloads.rlp")
	out := filepath.Join(dir, "payloads.rlp.cohered")

	all := canonicalBlocks(4)
	// Block 3 present only as an orphan (does not link); cohere must refetch it.
	writePayloadsFile(t, in, []*payloads.ExecutionPayloadV3{
		all[1], all[2], orphanPayload(3), all[4],
	})

	summary, err := Cohere(context.Background(), CohereConfig{
		InputPath: in, OutputPath: out, From: 1, Head: 4,
		Fetcher: cohereFetcher(t, all, 4),
	})
	if err != nil {
		t.Fatalf("Cohere: %v", err)
	}
	if got := readPayloadNumbers(t, out); !equalU64(got, []uint64{1, 2, 3, 4}) {
		t.Fatalf("blocks: got %v want [1 2 3 4]", got)
	}
	if summary.Relinked != 1 {
		t.Errorf("Relinked: got %d want 1", summary.Relinked)
	}
	if summary.Reused != 3 { // 1,2,4
		t.Errorf("Reused: got %d want 3", summary.Reused)
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
