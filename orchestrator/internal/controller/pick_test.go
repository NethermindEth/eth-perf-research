package controller

import (
	"math"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

// TestPickGasburnertxRespectsBlockGasLimit exercises the gas-budget cap.
// gasburnertx is the worst-case verb (~1.5M gas/tx); with an 8 GGas block
// limit the dispatcher's hard 0.95 ceiling is 7.6 GGas, which fits at most
// ~5066 txs before the safety margin. Pick must size NMaxTxs so the actual
// dispatched gas never exceeds 0.95 * blockGasLimit.
func TestPickGasburnertxRespectsBlockGasLimit(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000

	verbs := []string{"gasburnertx"}
	// Use a very high F to encourage Pick to choose this verb; with one verb
	// in the list it's the only option anyway.
	rf := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{"gasburnertx": {"accounts": 100, "storage": 100, "code": 100}},
		AvgTxRLP: map[string]float64{"gasburnertx": 1500.0},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	tgt := makeTarget(10 * 1024 * 1024 * 1024) // 10 GiB target — far from done
	obs := zeroObs()

	plan := s.Pick(obs, tgt, 8*1024*1024 /* total_batch_bytes */, blockGasLimit)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.Verb != "gasburnertx" {
		t.Fatalf("verb = %q, want gasburnertx", plan.Verb)
	}
	if plan.NMaxTxs <= 0 {
		t.Fatalf("NMaxTxs = %d, want > 0", plan.NMaxTxs)
	}

	perTx, ok := baseGasPerVerb["gasburnertx"]
	if !ok {
		t.Fatal("baseGasPerVerb[\"gasburnertx\"] not registered")
	}
	totalGas := uint64(plan.NMaxTxs) * perTx
	ceiling := uint64(float64(blockGasLimit) * gasCapFraction)
	if totalGas > ceiling {
		t.Fatalf("Pick over-allocated: NMaxTxs=%d * %d = %d gas > 0.95 * %d = %d ceiling",
			plan.NMaxTxs, perTx, totalGas, blockGasLimit, ceiling)
	}
}

// TestPickGasPrimarySizerTargetsBlockFill is the regression test for the
// gas-fill fix: gas is now the PRIMARY batch sizer. For a mid-cost verb that
// is gas-bound (not byte-bound), Pick must size NMaxTxs so the batch targets
// ≥85% of the block gas limit (the old byte-dominant sizing left blocks ~13%
// full) while never exceeding the dispatcher's 0.95 hard ceiling.
func TestPickGasPrimarySizerTargetsBlockFill(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000

	verbs := []string{"storagespam"}
	rf := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{"storagespam": {"accounts": 100, "storage": 100, "code": 100}},
		AvgTxRLP: map[string]float64{"storagespam": 1500.0},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	tgt := makeTarget(10 * 1024 * 1024 * 1024) // 10 GiB target — far from done
	plan := s.Pick(zeroObs(), tgt, 8*1024*1024 /* total_batch_bytes */, blockGasLimit)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.Verb != "storagespam" {
		t.Fatalf("verb = %q, want storagespam", plan.Verb)
	}

	perTx, ok := baseGasPerVerb["storagespam"]
	if !ok {
		t.Fatal("baseGasPerVerb[\"storagespam\"] not registered")
	}
	totalGas := uint64(plan.NMaxTxs) * perTx
	lowerBound := uint64(float64(blockGasLimit) * 0.85)
	upperBound := uint64(float64(blockGasLimit) * gasCapFraction)
	if totalGas < lowerBound {
		t.Fatalf("under-filled: NMaxTxs=%d * %d = %d gas < 0.85 * %d = %d (gas must be the primary sizer)",
			plan.NMaxTxs, perTx, totalGas, blockGasLimit, lowerBound)
	}
	if totalGas > upperBound {
		t.Fatalf("over-allocated: NMaxTxs=%d * %d = %d gas > 0.95 * %d = %d ceiling",
			plan.NMaxTxs, perTx, totalGas, blockGasLimit, upperBound)
	}
}

// TestPickZeroBlockGasLimitDoesNotCap: when the lifecycle hasn't yet refreshed
// the block gas limit (e.g. very first batch on a fresh client), Pick must
// still produce a usable plan rather than wedging at NMaxTxs=0.
func TestPickZeroBlockGasLimitDoesNotCap(t *testing.T) {
	verbs := []string{"eoatx"}
	rf := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{"eoatx": {"accounts": 200, "storage": 5, "code": 5}},
		AvgTxRLP: map[string]float64{"eoatx": 1500.0},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	plan := s.Pick(zeroObs(), makeTarget(10_000_000), 4_000_000, 0)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.NMaxTxs <= 0 {
		t.Fatalf("NMaxTxs = %d, want > 0 (zero gas limit should not cap)", plan.NMaxTxs)
	}
}

// TestApplyCheapBloatBiasFavoursCheapByteVerb: two learned verbs with equal
// byte yield but different ETH cost — the cheaper one (eoatx, 21k gas baseline)
// must out-weigh the expensive one (storagespam, 2.5M gas baseline) because it
// delivers more state-bytes per ETH spent.
func TestApplyCheapBloatBiasFavoursCheapByteVerb(t *testing.T) {
	verbs := []string{"eoatx", "storagespam"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			// Equal byte yield (1500 bytes/tx total) so only ETH cost differs.
			"eoatx":       {"accounts": 1500, "storage": 0, "code": 0},
			"storagespam": {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "storagespam": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	xProj := []float64{0.5, 0.5}
	out := applyCheapBloatBias(verbs, xProj, s)

	// eoatx baseline 21k gas vs storagespam 2.5M gas → eoatx has ~119x the
	// bytes-per-ETH, so its post-bias weight must dominate.
	if !(out[0] > out[1]) {
		t.Errorf("eoatx weight=%.6f must exceed storagespam weight=%.6f (cheaper per byte)", out[0], out[1])
	}
	if sum := out[0] + out[1]; math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("bias output not on simplex: sum=%.9f", sum)
	}
}

// TestApplyCheapBloatBiasNeutralForUnlearnedVerb: a verb with zero F (never
// observed) must keep its xProj weight — the neutral-1.0 anti-deadlock rule.
// It must NOT be squashed to ~0 the way the old applyGasAwareBias did, since
// that is exactly what let noop win the mix permanently at cold start.
func TestApplyCheapBloatBiasNeutralForUnlearnedVerb(t *testing.T) {
	verbs := []string{"eoatx", "newverb"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			// eoatx is learned; newverb has zero F (unlearned).
			"eoatx":   {"accounts": 1500, "storage": 0, "code": 0},
			"newverb": {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "newverb": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	xProj := []float64{0.5, 0.5}
	out := applyCheapBloatBias(verbs, xProj, s)

	// newverb is unlearned → neutral multiplier. With only one learned verb the
	// mean equals eoatx's bytesPerEth so eoatx's multiplier is also 1.0, leaving
	// the mix unchanged. The key invariant: newverb keeps a non-trivial weight.
	if out[1] < 0.1 {
		t.Errorf("unlearned newverb weight=%.6f, want >= 0.1 (neutral-1.0 anti-deadlock rule)", out[1])
	}
	if sum := out[0] + out[1]; math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("bias output not on simplex: sum=%.9f", sum)
	}
}

// TestApplyCheapBloatBiasColdStartIsNoOp: when every verb is unlearned (all F
// zero, the live cold-start condition) the bias passes xProj through unchanged
// so ε-greedy explores uniformly and the verbs run — no deadlock.
func TestApplyCheapBloatBiasColdStartIsNoOp(t *testing.T) {
	verbs := []string{"eoatx", "storagespam", "gasburnertx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":       {"accounts": 0, "storage": 0, "code": 0},
			"storagespam": {"accounts": 0, "storage": 0, "code": 0},
			"gasburnertx": {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "storagespam": 1500, "gasburnertx": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	xProj := []float64{0.2, 0.3, 0.5}
	out := applyCheapBloatBias(verbs, xProj, s)
	for i := range xProj {
		if math.Abs(out[i]-xProj[i]) > 1e-9 {
			t.Errorf("cold-start bias altered weight[%d]: got %.9f, want %.9f", i, out[i], xProj[i])
		}
	}
}

// TestApplyCheapBloatBiasPreservesNoop: noop is the idle fallback; its xProj
// weight must pass through untouched even when other verbs are re-weighted.
func TestApplyCheapBloatBiasPreservesNoop(t *testing.T) {
	verbs := []string{"eoatx", "storagespam", "noop"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":       {"accounts": 1500, "storage": 0, "code": 0},
			"storagespam": {"accounts": 0, "storage": 1500, "code": 0},
			"noop":        {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "storagespam": 1500, "noop": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0, 1_000_000_000)

	xProj := []float64{0.4, 0.4, 0.2}
	out := applyCheapBloatBias(verbs, xProj, s)
	if sum := out[0] + out[1] + out[2]; math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("bias output not on simplex: sum=%.9f", sum)
	}
	// noop is re-weighted from 0.2 only by the simplex re-normalisation; eoatx
	// must still dominate storagespam on the cheap-bloat criterion.
	if !(out[0] > out[1]) {
		t.Errorf("eoatx weight=%.6f must exceed storagespam weight=%.6f", out[0], out[1])
	}
}

// TestComputeGasBasedMaxTable spot-checks the gas-cap formula for the verbs
// most likely to hit the ceiling.
func TestComputeGasBasedMaxTable(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000
	ceiling := uint64(float64(blockGasLimit) * gasCapFraction)

	verbs := make([]string, 0, len(baseGasPerVerb))
	for v := range baseGasPerVerb {
		verbs = append(verbs, v)
	}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := NewState(verbs, ref, identity, 0.0, 1_000_000_000)

	for verb, perTx := range baseGasPerVerb {
		got := s.computeGasBasedMax(verb, blockGasLimit)
		if got <= 0 {
			t.Errorf("verb=%s: computeGasBasedMax=%d, want > 0", verb, got)
			continue
		}
		totalGas := uint64(got) * perTx
		if totalGas > ceiling {
			t.Errorf("verb=%s: max=%d * perTx=%d = %d > ceiling=%d",
				verb, got, perTx, totalGas, ceiling)
		}
	}
}
