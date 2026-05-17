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
	s := newTestState(verbs, rf, identity, 0.0)

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

	perTx, ok := testBaseGasPerVerb["gasburnertx"]
	if !ok {
		t.Fatal("testBaseGasPerVerb[\"gasburnertx\"] not registered")
	}
	totalGas := uint64(plan.NMaxTxs) * perTx
	ceiling := uint64(float64(blockGasLimit) * testGasCapFraction)
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
	s := newTestState(verbs, rf, identity, 0.0)

	tgt := makeTarget(10 * 1024 * 1024 * 1024) // 10 GiB target — far from done
	plan := s.Pick(zeroObs(), tgt, 8*1024*1024 /* total_batch_bytes */, blockGasLimit)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.Verb != "storagespam" {
		t.Fatalf("verb = %q, want storagespam", plan.Verb)
	}

	perTx, ok := testBaseGasPerVerb["storagespam"]
	if !ok {
		t.Fatal("testBaseGasPerVerb[\"storagespam\"] not registered")
	}
	totalGas := uint64(plan.NMaxTxs) * perTx
	lowerBound := uint64(float64(blockGasLimit) * 0.85)
	upperBound := uint64(float64(blockGasLimit) * testGasCapFraction)
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
	s := newTestState(verbs, rf, identity, 0.0)

	plan := s.Pick(zeroObs(), makeTarget(10_000_000), 4_000_000, 0)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.NMaxTxs <= 0 {
		t.Fatalf("NMaxTxs = %d, want > 0 (zero gas limit should not cap)", plan.NMaxTxs)
	}
}

// TestPickProjectedGradientSteersAwayFromOverTargetAxis is the regression test
// for the cheap-bloat-bias removal: with no post-projection verb-mix bias, Pick
// is pure projected-gradient. When one axis is already OVER its target share
// and another is UNDER, the gradient must weight the simplex toward the verb
// that fills the under-served axis — never toward the over-served one.
func TestPickProjectedGradientSteersAwayFromOverTargetAxis(t *testing.T) {
	// accountfiller only grows accounts; storagefiller only grows storage.
	verbs := []string{"accountfiller", "storagefiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountfiller": {"accounts": 1500, "storage": 0, "code": 0},
			"storagefiller": {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountfiller": 1500, "storagefiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.0)

	// Target: accounts 50% / storage 50%. Observation: storage far OVER, accounts
	// far UNDER — the residual must drive the mix toward accountfiller.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.Mix["accountfiller"] <= plan.Mix["storagefiller"] {
		t.Errorf("projected gradient must favour the under-served axis: "+
			"accountfiller weight=%.6f must exceed storagefiller weight=%.6f",
			plan.Mix["accountfiller"], plan.Mix["storagefiller"])
	}
}

// TestComputeGasBasedMaxTable spot-checks the gas-cap formula for the verbs
// most likely to hit the ceiling.
func TestComputeGasBasedMaxTable(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000
	ceiling := uint64(float64(blockGasLimit) * testGasCapFraction)

	verbs := make([]string, 0, len(testBaseGasPerVerb))
	for v := range testBaseGasPerVerb {
		verbs = append(verbs, v)
	}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := newTestState(verbs, ref, identity, 0.0)

	for verb, perTx := range testBaseGasPerVerb {
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

// TestComputeGasBasedMaxIgnoresMislearnedLowEWMA: a verb whose learned EWMA has
// mis-learned a gas value far BELOW its true cost (polluted by partially-
// included or rejected batches) must still be sized by the static baseline.
// gasPerTxEstimate returns max(EWMA, baseline), so the under-estimate can never
// oversize the batch — the production termination bug (20 consecutive "gas cap
// exceeded" skips) was caused by the EWMA overriding a correct baseline.
func TestComputeGasBasedMaxIgnoresMislearnedLowEWMA(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000
	const verb = "storagespam" // baseline ~2.5M gas/tx

	verbs := []string{verb}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := newTestState(verbs, ref, identity, 0.0)

	// Pollute the EWMA with a mis-learned LOW value (50k gas/tx, 50x too low)
	// across enough samples to pass the cold-start threshold.
	for i := 0; i < int(testVerbStatsColdStartN)+5; i++ {
		s.UpdateVerbStats(verb, 50_000, 1, 1500.0)
	}
	vs := s.GetVerbStats(verb)
	if vs == nil || vs.Samples < testVerbStatsColdStartN {
		t.Fatalf("EWMA not warm: samples=%v", vs)
	}
	if learned := vs.GasPerTx.Value(); learned >= float64(s.baselineGasPerVerb(verb)) {
		t.Fatalf("test setup: learned EWMA=%.0f not below baseline=%d",
			learned, s.baselineGasPerVerb(verb))
	}

	// The estimate must be clamped UP to the baseline, not the low EWMA.
	if est := s.gasPerTxEstimate(verb); est < float64(s.baselineGasPerVerb(verb)) {
		t.Errorf("gasPerTxEstimate=%.0f below baseline=%d — under-estimate not clamped",
			est, s.baselineGasPerVerb(verb))
	}

	// The resulting cap, priced at the BASELINE gas, must fit under the hard
	// 0.95 ceiling — i.e. the batch can never be oversized past dispatch limit.
	got := s.computeGasBasedMax(verb, blockGasLimit)
	if got <= 0 {
		t.Fatalf("computeGasBasedMax=%d, want > 0", got)
	}
	hardCeiling := uint64(float64(blockGasLimit) * testGasCapFraction)
	totalGas := uint64(got) * s.baselineGasPerVerb(verb)
	if totalGas > hardCeiling {
		t.Errorf("cap=%d * baselineGas=%d = %d > hard ceiling=%d — batch would be rejected",
			got, s.baselineGasPerVerb(verb), totalGas, hardCeiling)
	}
}
