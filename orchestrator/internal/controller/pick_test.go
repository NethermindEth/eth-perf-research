package controller

import (
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
	s := NewState(verbs, rf, identity, 0.0)

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
	s := NewState(verbs, rf, identity, 0.0)

	plan := s.Pick(zeroObs(), makeTarget(10_000_000), 4_000_000, 0)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.NMaxTxs <= 0 {
		t.Fatalf("NMaxTxs = %d, want > 0 (zero gas limit should not cap)", plan.NMaxTxs)
	}
}

// TestPickGasAwareBiasSquashesGasburnertx: with mock F values where eoatx has
// high bytes-per-gas and gasburnertx has zero useful bytes, the resulting mix
// must put gasburnertx well below 1% and eoatx must dominate storagespam (which
// has high gas cost per byte). This is the user-facing invariant of Fix 2:
// don't burn the master signer's ETH on zero-yield expensive verbs.
func TestPickGasAwareBiasSquashesGasburnertx(t *testing.T) {
	t.Setenv("ORCH_GAS_AWARE_EXPONENT", "0.5")

	verbs := []string{"eoatx", "storagespam", "gasburnertx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			// eoatx: 1500 bytes/tx total at 21k gas → ~0.071 B/gas (high)
			"eoatx": {"accounts": 1500, "storage": 0, "code": 0},
			// storagespam: 1500 bytes/tx total at 2.5M gas → 0.0006 B/gas (mid)
			"storagespam": {"accounts": 0, "storage": 1500, "code": 0},
			// gasburnertx: 0 useful bytes at 1.5M gas → 0 B/gas (zero)
			"gasburnertx": {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{
			"eoatx":       1500,
			"storagespam": 1500,
			"gasburnertx": 1500,
		},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0)

	plan := s.Pick(zeroObs(), makeTarget(10*1024*1024), 8*1024*1024, 8_000_000_000)
	if plan == nil {
		t.Fatal("Pick returned nil plan")
	}
	if plan.Mix == nil {
		t.Fatal("plan.Mix is nil")
	}

	mEoatx := plan.Mix["eoatx"]
	mStorage := plan.Mix["storagespam"]
	mGasburn := plan.Mix["gasburnertx"]

	if mGasburn >= 0.01 {
		t.Errorf("gasburnertx mix=%.6f, want < 0.01 (gas-aware bias should squash zero-byte verbs)", mGasburn)
	}
	if !(mEoatx > mStorage) {
		t.Errorf("eoatx mix=%.4f must exceed storagespam mix=%.4f (bytes-per-gas: eoatx >> storagespam)", mEoatx, mStorage)
	}
}

// TestPickGasAwareBiasDisabledByZeroExponent: with the exponent at 0 the mix
// must be (close to) the un-biased projection. Acts as a regression guard for
// the disable path and keeps backwards-compatible behaviour available.
func TestPickGasAwareBiasDisabledByZeroExponent(t *testing.T) {
	t.Setenv("ORCH_GAS_AWARE_EXPONENT", "0")

	verbs := []string{"eoatx", "gasburnertx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":       {"accounts": 100, "storage": 100, "code": 100},
			"gasburnertx": {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "gasburnertx": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0)

	plan := s.Pick(zeroObs(), makeTarget(10*1024*1024), 8*1024*1024, 8_000_000_000)
	if plan == nil {
		t.Fatal("Pick returned nil plan")
	}
	// At exp=0 the bias is disabled → gasburnertx keeps its un-biased weight.
	// The simplex projection for two equally-weighted verbs is ~(0.5, 0.5),
	// so gasburnertx should be non-trivial (> 0.1) here.
	if plan.Mix["gasburnertx"] < 0.1 {
		t.Errorf("exp=0 should disable bias; gasburnertx mix=%.4f want >= 0.1", plan.Mix["gasburnertx"])
	}
}

// TestPickGasAwareBiasAggressiveExponent: at exp=2 the bias is aggressive;
// gasburnertx must be near-zero and eoatx must dominate.
func TestPickGasAwareBiasAggressiveExponent(t *testing.T) {
	t.Setenv("ORCH_GAS_AWARE_EXPONENT", "2")

	verbs := []string{"eoatx", "gasburnertx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":       {"accounts": 1500, "storage": 0, "code": 0},
			"gasburnertx": {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "gasburnertx": 1500},
	}
	var identity [32]byte
	s := NewState(verbs, rf, identity, 0.0)

	plan := s.Pick(zeroObs(), makeTarget(10*1024*1024), 8*1024*1024, 8_000_000_000)
	if plan == nil {
		t.Fatal("Pick returned nil plan")
	}
	if plan.Mix["gasburnertx"] > 1e-6 {
		t.Errorf("exp=2: gasburnertx mix=%.9f, want ~0", plan.Mix["gasburnertx"])
	}
	if plan.Mix["eoatx"] < 0.99 {
		t.Errorf("exp=2: eoatx mix=%.4f, want > 0.99 (should dominate)", plan.Mix["eoatx"])
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
	s := NewState(verbs, ref, identity, 0.0)

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
