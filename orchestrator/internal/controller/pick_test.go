package controller

import (
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
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

// TestPickR1EntropyFloorKeepsSoleAxisGrower is the regression test for R1: a
// verb that is the ONLY non-degenerate grower of an under-target axis must
// never be zeroed by the Michelot projection — it must keep at least
// EntropyFloor weight so the ε-greedy branch can still reach it.
func TestPickR1EntropyFloorKeepsSoleAxisGrower(t *testing.T) {
	// codefiller is the sole grower of the code axis; calltx grows nothing but
	// is non-degenerate (touches accounts), so the gradient will heavily favour
	// the verbs serving the larger residuals and try to zero codefiller.
	verbs := []string{"calltx", "storagefiller", "codefiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"calltx":        {"accounts": 8, "storage": 0, "code": 0},
			"storagefiller": {"accounts": 0, "storage": 1500, "code": 0},
			"codefiller":    {"accounts": 0, "storage": 0, "code": 1500},
		},
		AvgTxRLP: map[string]float64{"calltx": 1500, "storagefiller": 1500, "codefiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.5)

	// code is far under-target; accounts/storage near their lines.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.1, AxisStorage: 0.1, AxisCode: 0.8},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 100_000_000, StorageTrieBytes: 100_000_000, CodeBytesTotal: 1_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	floor := config.Defaults().Control.EntropyFloor
	if plan.Mix["codefiller"] < floor {
		t.Errorf("R1: sole code-axis grower weight=%.6f, want >= EntropyFloor=%.3f",
			plan.Mix["codefiller"], floor)
	}
	for v, w := range plan.Mix {
		if w <= 0 {
			t.Errorf("R1: eligible verb %q zeroed (weight=%.6f)", v, w)
		}
	}
}

// TestPickR1EntropyFloorExemptsDegenerateVerb verifies a verb with an all-zero
// F-row (no measured effect) is NOT lifted by the entropy floor — it correctly
// stays at zero.
func TestPickR1EntropyFloorExemptsDegenerateVerb(t *testing.T) {
	verbs := []string{"accountfiller", "deadverb"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountfiller": {"accounts": 1500, "storage": 0, "code": 0},
			"deadverb":      {"accounts": 0, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountfiller": 1500, "deadverb": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.5)

	plan := s.Pick(zeroObs(), makeTarget(10*1024*1024*1024), 8*1024*1024, 8_000_000_000)
	if plan.Mix["deadverb"] != 0 {
		t.Errorf("R1: degenerate verb should stay at 0, got weight=%.6f", plan.Mix["deadverb"])
	}
}

// TestPickR2AntiWindupZeroesOverTargetAxisGradient is the regression test for
// R2: when an axis is over-target its gradient contribution is clamped to zero,
// so a verb that ONLY grows that over-target axis must not be penalised away by
// the over-target pressure — the controller stops fighting a satisfied axis.
func TestPickR2AntiWindupZeroesOverTargetAxisGradient(t *testing.T) {
	verbs := []string{"accountfiller", "storagefiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountfiller": {"accounts": 1500, "storage": 0, "code": 0},
			"storagefiller": {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountfiller": 1500, "storagefiller": 1500},
	}
	var identity [32]byte

	// storage far over-target, accounts far under-target.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	// With anti-windup ON the over-target storage axis contributes no gradient,
	// so storagefiller's weight is driven purely by the (zero) storage term and
	// stays near baseline rather than being pushed down.
	on := newTestState(verbs, rf, identity, 0.0)
	planOn := on.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)

	off := newTestState(verbs, rf, identity, 0.0)
	off.cfg.Control.AntiWindupEnabled = false
	planOff := off.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)

	// Anti-windup must reduce the away-pushing pressure: storagefiller keeps
	// more weight with the clamp on than off.
	if planOn.Mix["storagefiller"] <= planOff.Mix["storagefiller"] {
		t.Errorf("R2: anti-windup should not push the over-target verb down harder; "+
			"on=%.6f off=%.6f", planOn.Mix["storagefiller"], planOff.Mix["storagefiller"])
	}
	// The under-served axis must still be the controller's priority.
	if planOn.Mix["accountfiller"] <= planOn.Mix["storagefiller"] {
		t.Errorf("R2: under-served accounts must still dominate: acc=%.6f sto=%.6f",
			planOn.Mix["accountfiller"], planOn.Mix["storagefiller"])
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
