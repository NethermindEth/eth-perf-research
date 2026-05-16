package lifecycle

import (
	"errors"
	"testing"
)

// TestIsPartialAcceptanceErrMatchesNMFormat: the exact wire string NM emits
// when its block builder drops a few txs from a commit must be classified as
// partial-acceptance (soft skip), not as a fatal commit error.
func TestIsPartialAcceptanceErrMatchesNMFormat(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "NM canonical partial-acceptance",
			err:  errors.New("testing_commitBlockV1: rpc error: code=-32000 message=expected 45100 transactions but only 44653 were included"),
			want: true,
		},
		{
			name: "NM partial-acceptance, count 1",
			err:  errors.New("expected 100 transactions but only 1 were included"),
			want: true,
		},
		{
			name: "generic RPC failure",
			err:  errors.New("testing_commitBlockV1: rpc error: code=-32603 message=internal server error"),
			want: false,
		},
		{
			name: "context cancelled",
			err:  errors.New("context cancelled"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPartialAcceptanceErr(tc.err); got != tc.want {
				t.Fatalf("isPartialAcceptanceErr(%v)=%v want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestParsePartialAcceptanceExtractsCounts: the error string carries the
// expected/included counts; the warn log surfaces them to the operator so a
// sustained partial-acceptance pattern is visible.
func TestParsePartialAcceptanceExtractsCounts(t *testing.T) {
	err := errors.New("testing_commitBlockV1: rpc error: code=-32000 message=expected 45100 transactions but only 44653 were included")
	expected, included := parsePartialAcceptance(err)
	if expected != 45100 {
		t.Errorf("expected=%d want 45100", expected)
	}
	if included != 44653 {
		t.Errorf("included=%d want 44653", included)
	}
}

func TestParsePartialAcceptanceMalformedReturnsZeros(t *testing.T) {
	expected, included := parsePartialAcceptance(errors.New("totally unrelated error"))
	if expected != 0 || included != 0 {
		t.Fatalf("malformed: got (%d,%d) want (0,0)", expected, included)
	}
}

// TestCommitPartialAcceptanceSoftSkip simulates the runLoop's commit branch:
// on a partial-acceptance error the loop advances to the next batch, the run
// does not terminate, and the obs/lastBlock state does not move. This is the
// invariant Fix 1 establishes so a single NM partial-acceptance event does not
// bring down the whole run.
//
// We exercise the decision logic in isolation (the real loop integrates
// RPC/sensor/journal which would require ~hundreds of lines of mocking). The
// integration_test.go path covers full end-to-end behaviour.
func TestCommitPartialAcceptanceSoftSkip(t *testing.T) {
	// Three batches at batchIDs 0/1/2 where batch 1 fails with a
	// partial-acceptance error. The loop must drain all three and not halt.
	batchIDs := []uint64{0, 1, 2}

	commitErr := errors.New("testing_commitBlockV1: rpc error: code=-32000 message=expected 45100 transactions but only 44653 were included")

	// Stand-in for the real commit-side mutable state.
	var (
		processed   uint64
		terminated  bool
		obsUpdated  int
		skipsLogged int
	)

	// Replay the commit-loop logic for each batch in order.
	for _, batchID := range batchIDs {
		processed++

		// Simulate: batch 1 returns a partial-acceptance error; others succeed.
		var err error
		if batchID == 1 {
			err = commitErr
		}
		if err != nil {
			if isPartialAcceptanceErr(err) {
				expected, included := parsePartialAcceptance(err)
				if expected != 45100 || included != 44653 {
					t.Fatalf("parsePartialAcceptance: got (%d,%d) want (45100,44653)", expected, included)
				}
				skipsLogged++
				continue
			}
			// Genuine error path: would have triggered setTerm.
			terminated = true
			continue
		}
		obsUpdated++
	}

	if terminated {
		t.Fatal("run terminated on partial-acceptance; soft-skip contract violated")
	}
	if processed != 3 {
		t.Fatalf("processed=%d, want 3 (all batches drained)", processed)
	}
	if skipsLogged != 1 {
		t.Fatalf("skipsLogged=%d, want 1", skipsLogged)
	}
	if obsUpdated != 2 {
		t.Fatalf("obsUpdated=%d, want 2 (only batch 0 and 2 commit)", obsUpdated)
	}
}
