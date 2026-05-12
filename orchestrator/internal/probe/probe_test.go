package probe

import (
	"context"
	"errors"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// fakeRunner returns deterministic deltas per verb.
type fakeRunner struct {
	// perVerb maps verb -> (rlpBytes, accountDelta, storageDelta, codeDelta) per run.
	perVerb map[string]fakeResult
}

type fakeResult struct {
	rlpBytes     uint64
	accountDelta uint64
	storageDelta uint64
	codeDelta    uint64
}

func (f *fakeRunner) RunOneVerb(_ context.Context, verb string, _ int) (uint64, controller.Observation, controller.Observation, error) {
	r, ok := f.perVerb[verb]
	if !ok {
		return 0, controller.Observation{}, controller.Observation{}, errors.New("unknown verb: " + verb)
	}
	pre := controller.Observation{}
	post := controller.Observation{
		AccountTrieBytes: r.accountDelta,
		StorageTrieBytes: r.storageDelta,
		CodeBytesTotal:   r.codeDelta,
	}
	return r.rlpBytes, pre, post, nil
}

func makeTestRef(verbs []string, fVal float64) *referencef.ReferenceF {
	rf := &referencef.ReferenceF{
		Verbs:    make(map[string]map[string]float64, len(verbs)),
		AvgTxRLP: make(map[string]float64, len(verbs)),
	}
	for _, v := range verbs {
		rf.Verbs[v] = map[string]float64{
			"accounts": fVal,
			"storage":  fVal,
			"code":     fVal,
		}
	}
	return rf
}

// TestProbeComputesFAndAvgTxRLP: fake runner returning known deltas; assert
// F and AvgTxRLP match expected values.
func TestProbeComputesFAndAvgTxRLP(t *testing.T) {
	verbs := []string{"verb_a", "verb_b"}
	txCount := 10

	runner := &fakeRunner{
		perVerb: map[string]fakeResult{
			"verb_a": {rlpBytes: 15000, accountDelta: 1000, storageDelta: 500, codeDelta: 200},
			"verb_b": {rlpBytes: 20000, accountDelta: 2000, storageDelta: 800, codeDelta: 100},
		},
	}

	p := New(verbs, txCount, 0) // sanity gate disabled
	result, err := p.Run(context.Background(), runner, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// verb_a: measured = delta/txCount
	checkF(t, result, "verb_a", controller.AxisAccounts, 1000.0/10)
	checkF(t, result, "verb_a", controller.AxisStorage, 500.0/10)
	checkF(t, result, "verb_a", controller.AxisCode, 200.0/10)
	checkF(t, result, "verb_b", controller.AxisAccounts, 2000.0/10)
	checkF(t, result, "verb_b", controller.AxisStorage, 800.0/10)
	checkF(t, result, "verb_b", controller.AxisCode, 100.0/10)

	// AvgTxRLP = rlpBytes / txCount
	wantA := float64(15000) / 10
	if result.AvgTxRLP["verb_a"] != wantA {
		t.Fatalf("AvgTxRLP[verb_a]: got %v, want %v", result.AvgTxRLP["verb_a"], wantA)
	}
	wantB := float64(20000) / 10
	if result.AvgTxRLP["verb_b"] != wantB {
		t.Fatalf("AvgTxRLP[verb_b]: got %v, want %v", result.AvgTxRLP["verb_b"], wantB)
	}
}

// TestSanityGatePasses: probed values within 3× of reference must succeed.
func TestSanityGatePasses(t *testing.T) {
	verbs := []string{"verb_a"}
	txCount := 10

	runner := &fakeRunner{
		perVerb: map[string]fakeResult{
			// measured per-tx = 100/10 = 10; reference = 10 → ratio = 1.0 → passes
			"verb_a": {rlpBytes: 15000, accountDelta: 100, storageDelta: 100, codeDelta: 100},
		},
	}

	ref := makeTestRef(verbs, 10.0) // reference F = 10 per axis
	p := New(verbs, txCount, 3.0)
	_, err := p.Run(context.Background(), runner, ref)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// TestSanityGateTrips: probed values > 3× reference must return an error.
func TestSanityGateTrips(t *testing.T) {
	verbs := []string{"verb_a"}
	txCount := 10

	runner := &fakeRunner{
		perVerb: map[string]fakeResult{
			// measured per-tx = 500/10 = 50; reference = 10 → ratio = 5.0 → trips at 3×
			"verb_a": {rlpBytes: 15000, accountDelta: 500, storageDelta: 100, codeDelta: 100},
		},
	}

	ref := makeTestRef(verbs, 10.0)
	p := New(verbs, txCount, 3.0)
	_, err := p.Run(context.Background(), runner, ref)
	if err == nil {
		t.Fatal("expected sanity gate error, got nil")
	}
}

// TestSanityGateDisabledWhenZero: multiplier <= 0 skips the gate entirely.
func TestSanityGateDisabledWhenZero(t *testing.T) {
	verbs := []string{"verb_a"}
	txCount := 10

	runner := &fakeRunner{
		perVerb: map[string]fakeResult{
			// wildly off reference — gate should be suppressed
			"verb_a": {rlpBytes: 15000, accountDelta: 100_000, storageDelta: 0, codeDelta: 0},
		},
	}

	ref := makeTestRef(verbs, 10.0)
	p := New(verbs, txCount, -1) // negative → disabled
	_, err := p.Run(context.Background(), runner, ref)
	if err != nil {
		t.Fatalf("expected no error with gate disabled, got: %v", err)
	}
}

// TestRunnerErrorPropagates: if RunOneVerb fails the Probe must propagate the error.
func TestRunnerErrorPropagates(t *testing.T) {
	verbs := []string{"unknown_verb"}
	runner := &fakeRunner{perVerb: map[string]fakeResult{}} // no entry → will error
	p := New(verbs, 10, 0)
	_, err := p.Run(context.Background(), runner, nil)
	if err == nil {
		t.Fatal("expected error from runner, got nil")
	}
}

// TestContextCancellation: cancelled context causes Run to return immediately.
func TestContextCancellation(t *testing.T) {
	verbs := []string{"verb_a", "verb_b"}
	runner := &fakeRunner{
		perVerb: map[string]fakeResult{
			"verb_a": {rlpBytes: 1000, accountDelta: 10},
			"verb_b": {rlpBytes: 1000, accountDelta: 10},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	p := New(verbs, 10, 0)
	_, err := p.Run(ctx, runner, nil)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

func checkF(t *testing.T, r *Result, verb string, ax controller.Axis, want float64) {
	t.Helper()
	got := r.F[verb][ax]
	if got != want {
		t.Errorf("F[%s][%s]: got %v, want %v", verb, ax, got, want)
	}
}
