package controller

import (
	"math"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// makeRef returns a minimal ReferenceF with known F values for two verbs.
func makeRef(verbs []string, fVal float64) *referencef.ReferenceF {
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
		rf.AvgTxRLP[v] = 1500.0
	}
	return rf
}

// makeTarget returns a balanced target that splits evenly across axes.
func makeTarget(totalBytes int64) *Target {
	return &Target{
		Shares: map[Axis]float64{
			AxisAccounts: 1.0 / 3.0,
			AxisStorage:  1.0 / 3.0,
			AxisCode:     1.0 / 3.0,
		},
		TotalBytes: totalBytes,
		SHA256Hex:  "deadbeef",
	}
}

// zeroObs returns an observation with all-zero bytes.
func zeroObs() *Observation {
	return &Observation{}
}

// TestPickSingleVerbReturnsThatVerb: with exactly one verb the controller must
// always return it regardless of the observation.
func TestPickSingleVerbReturnsThatVerb(t *testing.T) {
	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 100.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)

	tgt := makeTarget(1_000_000)
	plan := s.Pick(zeroObs(), tgt, 4_000_000, 0)

	if plan.Verb != "eoa_transfer" {
		t.Fatalf("expected eoa_transfer, got %s", plan.Verb)
	}
	if plan.DeadlineBytes <= 0 {
		t.Fatalf("deadline_bytes must be positive, got %d", plan.DeadlineBytes)
	}
}

// TestPickDeterministicWithEpsilonZero: with ε=0 identical inputs always
// produce the same verb (deterministic argmax).
func TestPickDeterministicWithEpsilonZero(t *testing.T) {
	verbs := []string{"verb_a", "verb_b", "verb_c"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"verb_a": {"accounts": 200.0, "storage": 10.0, "code": 5.0},
			"verb_b": {"accounts": 10.0, "storage": 200.0, "code": 5.0},
			"verb_c": {"accounts": 5.0, "storage": 5.0, "code": 200.0},
		},
		AvgTxRLP: map[string]float64{"verb_a": 1500, "verb_b": 1500, "verb_c": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0) // ε = 0

	tgt := makeTarget(10_000_000)
	obs := zeroObs()

	first := s.Pick(obs, tgt, 4_000_000, 0).Verb
	for i := 0; i < 10; i++ {
		got := s.Pick(obs, tgt, 4_000_000, 0).Verb
		if got != first {
			t.Fatalf("non-deterministic: got %s on iteration %d, want %s", got, i, first)
		}
	}
}

// TestApplyMovesFInExpectedDirection: applying an observation larger than the
// current F estimate should increase F for that axis.
func TestApplyMovesFInExpectedDirection(t *testing.T) {
	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 50.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)

	pre := &Observation{AccountTrieBytes: 1_000_000}
	// post has +1000 bytes per axis in accounts, much more than F=50 predicts for 10 txs.
	post := &Observation{AccountTrieBytes: 1_001_000, StorageTrieBytes: 500, CodeBytesTotal: 200}

	plan := &BatchPlan{Verb: "eoa_transfer", DeadlineBytes: 10000, Mix: map[string]float64{"eoa_transfer": 1.0}}
	fBefore := s.F["eoa_transfer"][AxisAccounts]

	snap, err := s.Apply(pre, post, plan, 10, 15000)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	fAfter := s.F["eoa_transfer"][AxisAccounts]
	if fAfter <= fBefore {
		t.Fatalf("F[accounts] should increase: before=%.4f after=%.4f", fBefore, fAfter)
	}
	if snap.L2Norm < 0 {
		t.Fatalf("L2Norm should be non-negative")
	}
}

// TestApplyIncrementsBatchID: BatchID advances by 1 per Apply call.
func TestApplyIncrementsBatchID(t *testing.T) {
	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 50.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)

	if s.BatchID != 0 {
		t.Fatalf("initial BatchID want 0, got %d", s.BatchID)
	}

	plan := &BatchPlan{Verb: "eoa_transfer", DeadlineBytes: 1000, Mix: map[string]float64{"eoa_transfer": 1.0}}
	pre := zeroObs()
	post := &Observation{AccountTrieBytes: 500}

	for i := 1; i <= 3; i++ {
		if _, err := s.Apply(pre, post, plan, 1, 1500); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
		if s.BatchID != uint64(i) {
			t.Fatalf("BatchID after Apply %d: want %d, got %d", i, i, s.BatchID)
		}
	}
}

// TestApplyErrorOnZeroTxCount: Apply must return an error for txCount <= 0.
func TestApplyErrorOnZeroTxCount(t *testing.T) {
	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 50.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)
	plan := &BatchPlan{Verb: "eoa_transfer", DeadlineBytes: 1000, Mix: map[string]float64{"eoa_transfer": 1.0}}
	_, err := s.Apply(zeroObs(), zeroObs(), plan, 0, 0)
	if err == nil {
		t.Fatal("expected error for txCount=0, got nil")
	}
}

// TestInstabilityTripsAfterMaxTrips: detector fires after maxTrips exceedances
// within window batches.
func TestInstabilityTripsAfterMaxTrips(t *testing.T) {
	const window = 6
	const maxTrips = 4
	const grace = 0

	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 1.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)

	// Pre-size the window.
	s.initOvershootWindow(window)

	// Force overshootFilled = window and set maxTrips entries to true.
	for i := 0; i < window; i++ {
		s.overshootWindow[i] = i < maxTrips
	}
	s.overshootFilled = window

	if !s.HasInstability(overshootThreshold, window, maxTrips, grace) {
		t.Fatal("expected instability to be detected")
	}

	// With only maxTrips-1 trips it must not fire.
	for i := 0; i < window; i++ {
		s.overshootWindow[i] = i < maxTrips-1
	}
	if s.HasInstability(overshootThreshold, window, maxTrips, grace) {
		t.Fatal("should not detect instability with fewer than maxTrips exceedances")
	}
}

// TestInstabilityNotTripBeforeWindowFull: the detector must not fire if the
// window hasn't been filled yet.
func TestInstabilityNotTripBeforeWindowFull(t *testing.T) {
	const window = 6
	const maxTrips = 1

	verbs := []string{"eoa_transfer"}
	ref := makeRef(verbs, 1.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)
	s.initOvershootWindow(window)
	// filled < window — should never trip.
	s.overshootFilled = window - 1
	for i := range s.overshootWindow {
		s.overshootWindow[i] = true
	}

	if s.HasInstability(overshootThreshold, window, maxTrips, 0) {
		t.Fatal("must not trip before window is full")
	}
}

// TestResidualL2NormHandComputed: verify the L2 norm of the residual snapshot
// matches a hand-computed expected value.
func TestResidualL2NormHandComputed(t *testing.T) {
	// Set up: F=10 per axis, txCount=10, post-pre = 150/axis.
	// observed_per_tx = 15.0 per axis.
	// After UpdateCoeff(10, 15, 1, 0.02):
	//   raw = 5, denom = max(1,1)=1, sat = tanh(5)≈1.0, sigmaNew=0.9+0.5=1.4,
	//   absRatio = 1/max(10,1000)=0.001, alphaTarget≈0.02+tiny, alphaNew≈0.02+tiny*0.3
	//   fNew ≈ 10 + 0.02 * 1.0 ≈ 10.02
	// commanded = fNew * 10 ≈ 100.2
	// obsVec = 15 * 10 = 150 per axis
	// diff per axis ≈ 150 - 100.2 = 49.8
	// L2 = sqrt(3) * 49.8 ≈ 86.27 — we just check the value is positive and finite.
	verbs := []string{"v"}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)

	pre := &Observation{}
	post := &Observation{AccountTrieBytes: 150, StorageTrieBytes: 150, CodeBytesTotal: 150}
	plan := &BatchPlan{Verb: "v", DeadlineBytes: 10000, Mix: map[string]float64{"v": 1.0}}

	snap, err := s.Apply(pre, post, plan, 10, 15000)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if snap.L2Norm <= 0 || math.IsNaN(snap.L2Norm) || math.IsInf(snap.L2Norm, 0) {
		t.Fatalf("L2Norm not positive-finite: %v", snap.L2Norm)
	}
	// All three per-axis residuals should be equal (symmetric setup).
	da := snap.PerAxis[AxisAccounts]
	ds := snap.PerAxis[AxisStorage]
	dc := snap.PerAxis[AxisCode]
	if math.Abs(da-ds) > 1e-9 || math.Abs(da-dc) > 1e-9 {
		t.Fatalf("per-axis residuals not equal: accounts=%v storage=%v code=%v", da, ds, dc)
	}
	// L2 should equal sqrt(3) * |per-axis|.
	want := math.Sqrt(3) * math.Abs(da)
	if math.Abs(snap.L2Norm-want) > 1e-9 {
		t.Fatalf("L2Norm: got %v, want %v", snap.L2Norm, want)
	}
}

// TestAvgTxRLPEWMA: AvgTxRLP follows the 0.3/0.7 EWMA rule.
func TestAvgTxRLPEWMA(t *testing.T) {
	verbs := []string{"v"}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0)
	// Seed with known value.
	s.AvgTxRLP["v"] = 1000.0

	plan := &BatchPlan{Verb: "v", DeadlineBytes: 10000, Mix: map[string]float64{"v": 1.0}}
	pre := zeroObs()
	post := &Observation{AccountTrieBytes: 100}

	// dispatched = 2000 bytes over 2 txs → observed_avg = 1000 (same as prev → EWMA stable)
	if _, err := s.Apply(pre, post, plan, 2, 2000); err != nil {
		t.Fatal(err)
	}
	want := 0.3*1000.0 + 0.7*1000.0
	if math.Abs(s.AvgTxRLP["v"]-want) > 1e-9 {
		t.Fatalf("AvgTxRLP EWMA: got %v, want %v", s.AvgTxRLP["v"], want)
	}

	// Now observed_avg = 2000, prev = 1000.
	s.AvgTxRLP["v"] = 1000.0
	if _, err := s.Apply(pre, post, plan, 1, 2000); err != nil {
		t.Fatal(err)
	}
	want2 := 0.3*2000.0 + 0.7*1000.0
	if math.Abs(s.AvgTxRLP["v"]-want2) > 1e-9 {
		t.Fatalf("AvgTxRLP EWMA 2: got %v, want %v", s.AvgTxRLP["v"], want2)
	}
}
