package replaycommit

import (
	"context"
	"errors"
	"io"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// fakeCommitter records commit calls and returns deterministic clean blocks.
// Each committed block's hash/stateRoot derive from the input timestamp so the
// "clean" root provably differs from the input's poisoned root.
type fakeCommitter struct {
	head         uint64
	commits      [][][]byte // signedTxs per commit, in order
	commitTS     []uint64
	failCommitAt int // 1-based index to fail at; 0 = never
}

func cleanHash(ts uint64) common.Hash {
	var h common.Hash
	big.NewInt(int64(ts)).FillBytes(h[:])
	h[0] = 0xCC // mark as "clean"
	return h
}

func (f *fakeCommitter) TestingCommitBlockV1(_ context.Context, signedTxs [][]byte, timestamp uint64) (common.Hash, error) {
	f.commits = append(f.commits, signedTxs)
	f.commitTS = append(f.commitTS, timestamp)
	if f.failCommitAt != 0 && len(f.commits) == f.failCommitAt {
		return common.Hash{}, errors.New("synthetic commit failure")
	}
	return cleanHash(timestamp), nil
}

func (f *fakeCommitter) BlockByHash(_ context.Context, h common.Hash, _ bool) (*rpc.BlockHeader, error) {
	// Recover the timestamp encoded into the hash to synthesize a block whose
	// number tracks how many we've committed.
	num := f.head + uint64(len(f.commits))
	return &rpc.BlockHeader{
		Number:    num,
		Hash:      h,
		StateRoot: h, // clean root == committed hash (0xCC...-prefixed)
		BaseFee:   big.NewInt(7),
	}, nil
}

func (f *fakeCommitter) Call(_ context.Context, method string, _ []any, out any) error {
	if method != "eth_blockNumber" {
		return errors.New("unexpected method " + method)
	}
	p, ok := out.(*string)
	if !ok {
		return errors.New("eth_blockNumber out is not *string")
	}
	// hex-encode f.head
	*p = "0x" + bigHex(f.head)
	return nil
}

func bigHex(n uint64) string {
	if n == 0 {
		return "0"
	}
	return big.NewInt(int64(n)).Text(16)
}

// sliceReader yields a fixed slice of payloads then io.EOF.
type sliceReader struct {
	ps  []*payloads.ExecutionPayloadV3
	idx int
}

func (s *sliceReader) Next() (*payloads.ExecutionPayloadV3, error) {
	if s.idx >= len(s.ps) {
		return nil, io.EOF
	}
	p := s.ps[s.idx]
	s.idx++
	return p, nil
}

type sliceWriter struct {
	appended []*payloads.ExecutionPayloadV3
}

func (w *sliceWriter) Append(p *payloads.ExecutionPayloadV3) error {
	w.appended = append(w.appended, p)
	return nil
}

// poisonPayload builds an input payload whose stateRoot is "poisoned"
// (0xDE...-prefixed) and distinct from the clean root the committer produces.
func poisonPayload(number, ts uint64) *payloads.ExecutionPayloadV3 {
	var poison common.Hash
	poison[0] = 0xDE
	poison[31] = byte(number)
	return &payloads.ExecutionPayloadV3{
		Number:       number,
		Timestamp:    ts,
		StateRoot:    poison,
		Transactions: [][]byte{{0x02, byte(number)}},
	}
}

func TestRunCommitsAllAndDropsPoisonRoot(t *testing.T) {
	in := &sliceReader{ps: []*payloads.ExecutionPayloadV3{
		poisonPayload(1, 1000),
		poisonPayload(2, 1001),
		poisonPayload(3, 1002),
	}}
	out := &sliceWriter{}
	fc := &fakeCommitter{head: 0}
	d := &Driver{Client: fc}

	if err := d.Run(context.Background(), in, out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(out.appended) != 3 {
		t.Fatalf("appended=%d want 3", len(out.appended))
	}
	if len(fc.commits) != 3 {
		t.Fatalf("commits=%d want 3", len(fc.commits))
	}
	for i, p := range out.appended {
		// Each output payload must carry the CLEAN root (0xCC-prefixed), never
		// the input's poisoned 0xDE-prefixed root.
		if p.StateRoot[0] != 0xCC {
			t.Errorf("output[%d] stateRoot=%s not a clean root", i, p.StateRoot.Hex())
		}
		if p.StateRoot[0] == 0xDE {
			t.Errorf("output[%d] carried poisoned root", i)
		}
		// Timestamp must be passed through verbatim.
		if fc.commitTS[i] != 1000+uint64(i) {
			t.Errorf("commit[%d] ts=%d want %d", i, fc.commitTS[i], 1000+uint64(i))
		}
	}
}

func TestRunSkipsAtOrBelowHead(t *testing.T) {
	in := &sliceReader{ps: []*payloads.ExecutionPayloadV3{
		poisonPayload(1, 1000), // <= head 2 -> skip
		poisonPayload(2, 1001), // == head 2 -> skip
		poisonPayload(3, 1002), // > head 2 -> commit
		poisonPayload(4, 1003), // > head 2 -> commit
	}}
	out := &sliceWriter{}
	fc := &fakeCommitter{head: 2}
	d := &Driver{Client: fc}

	if err := d.Run(context.Background(), in, out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fc.commits) != 2 {
		t.Fatalf("commits=%d want 2 (head=2 skips first two)", len(fc.commits))
	}
	if len(out.appended) != 2 {
		t.Fatalf("appended=%d want 2", len(out.appended))
	}
}

func TestRunFailsLoudOnCommitError(t *testing.T) {
	in := &sliceReader{ps: []*payloads.ExecutionPayloadV3{
		poisonPayload(1, 1000),
		poisonPayload(2, 1001),
	}}
	out := &sliceWriter{}
	fc := &fakeCommitter{head: 0, failCommitAt: 2}
	d := &Driver{Client: fc}

	err := d.Run(context.Background(), in, out)
	if err == nil {
		t.Fatal("expected error from commit failure, got nil")
	}
	if len(out.appended) != 1 {
		t.Fatalf("appended=%d want 1 (first ok, second failed)", len(out.appended))
	}
}

func TestRunReturnsNilOnCancel(t *testing.T) {
	in := &sliceReader{ps: []*payloads.ExecutionPayloadV3{
		poisonPayload(1, 1000),
	}}
	out := &sliceWriter{}
	fc := &fakeCommitter{head: 0}
	d := &Driver{Client: fc}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled; the post-commit ctx.Err() check returns nil

	if err := d.Run(ctx, in, out); err != nil {
		t.Fatalf("cancelled Run should return nil, got %v", err)
	}
}
