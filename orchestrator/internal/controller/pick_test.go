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

// TestPickBytesPerGasTieBreakPrefersEfficientVerb is the regression test for
// Change 4: among verbs the gradient deems equivalent (selection weights
// within ToleranceFloor), Pick must select the one with the higher learned
// BytesPerGas — the cheaper bloating choice.
func TestPickBytesPerGasTieBreakPrefersEfficientVerb(t *testing.T) {
	// Two verbs with IDENTICAL F-rows → identical gradient → equal weights.
	verbs := []string{"cheapfiller", "pricyfiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"cheapfiller": {"accounts": 100, "storage": 100, "code": 100},
			"pricyfiller": {"accounts": 100, "storage": 100, "code": 100},
		},
		AvgTxRLP: map[string]float64{"cheapfiller": 1500, "pricyfiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.0) // ε=0 → deterministic argmax

	// Warm BytesPerGas: cheapfiller emits far more bytes per gas than pricyfiller.
	for i := 0; i < int(testVerbStatsColdStartN)+5; i++ {
		s.UpdateVerbStats("cheapfiller", 100_000, 1, 5000.0) // 0.05 bytes/gas
		s.UpdateVerbStats("pricyfiller", 100_000, 1, 500.0)  // 0.005 bytes/gas
	}

	plan := s.Pick(zeroObs(), makeTarget(10*1024*1024*1024), 8*1024*1024, 8_000_000_000)
	if plan.Verb != "cheapfiller" {
		t.Errorf("tie-break: verb = %q, want cheapfiller (higher BytesPerGas)", plan.Verb)
	}
}

// TestPickBytesPerGasTieBreakNeverOverridesGradient verifies the tie-break is
// strictly a tie-breaker: a verb the gradient ranks clearly higher must win
// even if a low-BytesPerGas verb, never the reverse.
func TestPickBytesPerGasTieBreakNeverOverridesGradient(t *testing.T) {
	// gradverb is the clear gradient winner (only grower of the under-target
	// axis); effverb has a far higher BytesPerGas but a gradient-inferior F-row.
	verbs := []string{"gradverb", "effverb"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"gradverb": {"accounts": 1500, "storage": 0, "code": 0},
			"effverb":  {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"gradverb": 1500, "effverb": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.0)

	// effverb has a hugely better BytesPerGas — must still NOT be chosen.
	for i := 0; i < int(testVerbStatsColdStartN)+5; i++ {
		s.UpdateVerbStats("gradverb", 100_000, 1, 100.0)     // 0.001 bytes/gas
		s.UpdateVerbStats("effverb", 100_000, 1, 100_000.0)  // 1.0 bytes/gas
	}

	// accounts far under-target, storage far over-target → gradient strongly
	// favours gradverb; the two weights are NOT within ToleranceFloor.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "gradverb" {
		t.Errorf("tie-break overrode gradient: verb = %q, want gradverb "+
			"(gradient-superior verb must win despite lower BytesPerGas)", plan.Verb)
	}
}

// TestPickExcludesContractIneligibleVerb is the regression test for the
// verb-selection bug: a verb with the most attractive seed gradient must NOT
// be selected when its contract dependency is undeployed. uniswap_swaps has a
// storage-heavy F-row and would dominate the argmax, but with eligibility set
// to only eoatx it must be excluded from the candidate set entirely.
func TestPickExcludesContractIneligibleVerb(t *testing.T) {
	verbs := []string{"eoatx", "uniswap_swaps"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			// uniswap_swaps gets the largest F-row on every axis so its raw
			// gradient is the most attractive; only eligibility can keep it out.
			"eoatx":         {"accounts": 8, "storage": 5, "code": 0},
			"uniswap_swaps": {"accounts": 8, "storage": 220, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "uniswap_swaps": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.0) // ε=0 → deterministic argmax

	// Only eoatx is contract-eligible; uniswap_swaps' router is not deployed.
	s.SetEligibleVerbs([]string{"eoatx"})

	tgt := makeTarget(10 * 1024 * 1024 * 1024)
	for i := 0; i < 20; i++ {
		s.BatchID = uint64(i)
		plan := s.Pick(zeroObs(), tgt, 8*1024*1024, 8_000_000_000)
		if plan.Verb != "eoatx" {
			t.Fatalf("batch %d: verb = %q, want eoatx (ineligible verb must never be selected)", i, plan.Verb)
		}
		if w := plan.Mix["uniswap_swaps"]; w != 0 {
			t.Fatalf("batch %d: ineligible verb has weight %.6f, want 0", i, w)
		}
	}
}

// TestPickSelectsVerbOnceDependencyDeployed verifies the eligibility gate is
// not a permanent ban: the same verb becomes selectable once its contract is
// marked deployed. This proves the gate keys off the deployed set, not a
// hardcoded exclusion of uniswap_swaps.
func TestPickSelectsVerbOnceDependencyDeployed(t *testing.T) {
	verbs := []string{"eoatx", "uniswap_swaps"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":         {"accounts": 8, "storage": 5, "code": 0},
			"uniswap_swaps": {"accounts": 8, "storage": 220, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "uniswap_swaps": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.0)

	// Storage far under-target so the storage-heavy uniswap_swaps is the clear
	// gradient winner — it must be picked once its dependency is deployed.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.1, AxisStorage: 0.9, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := zeroObs()

	s.SetEligibleVerbs([]string{"eoatx", "uniswap_swaps"})
	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "uniswap_swaps" {
		t.Fatalf("verb = %q, want uniswap_swaps once its dependency is deployed", plan.Verb)
	}
	if w := plan.Mix["uniswap_swaps"]; w <= 0 {
		t.Fatalf("eligible uniswap_swaps has weight %.6f, want > 0", w)
	}
}

// TestPickR2KeepsOverTargetStorageVerbUnattractive is the R2 reproduction
// test for the live scenario: accounts under-target, storage over-target, and
// a verb with the live uniswap F-row [8,220,0]. R2 (per-axis anti-windup)
// clamps the over-target storage axis's gradient contribution to zero, so a
// storage-heavy verb must NOT out-score an account-driven verb purely because
// the over-target storage axis is large. Asserting on Pick's Mix exercises R2
// through the same path verb scoring uses.
func TestPickR2KeepsOverTargetStorageVerbUnattractive(t *testing.T) {
	// storageheavy mirrors the live uniswap_swaps seed F-row [8,220,0];
	// accountverb only meaningfully grows the under-target accounts axis.
	verbs := []string{"accountverb", "storageheavy"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountverb":  {"accounts": 160, "storage": 0, "code": 0},
			"storageheavy": {"accounts": 8, "storage": 220, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountverb": 1500, "storageheavy": 1500},
	}
	var identity [32]byte

	// accounts far under-target, storage far over-target — the live scenario.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	// With R2 ON the over-target storage axis contributes no gradient, so
	// storageheavy is scored only by its small accounts coefficient (8) vs
	// accountverb's 160 — the under-target account verb must dominate.
	s := newTestState(verbs, rf, identity, 0.0)
	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Mix["storageheavy"] >= plan.Mix["accountverb"] {
		t.Errorf("R2 broken: over-target storage-heavy verb weight=%.6f >= "+
			"account-driven verb weight=%.6f — a satisfied axis still attracts",
			plan.Mix["storageheavy"], plan.Mix["accountverb"])
	}
	if plan.Verb != "accountverb" {
		t.Errorf("R2: selected verb=%q, want accountverb (under-target axis must win)", plan.Verb)
	}

	// Cross-check: with R2 OFF the over-target storage axis DOES contribute, so
	// storageheavy keeps more weight. This confirms the test exercises R2
	// rather than passing trivially on the F-row asymmetry alone.
	off := newTestState(verbs, rf, identity, 0.0)
	off.cfg.Control.AntiWindupEnabled = false
	planOff := off.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if planOff.Mix["storageheavy"] <= plan.Mix["storageheavy"] {
		t.Errorf("R2 inert: storage-heavy weight with anti-windup off=%.6f "+
			"is not larger than with it on=%.6f", planOff.Mix["storageheavy"], plan.Mix["storageheavy"])
	}
}

// zeroLearnedFRow clears a verb's live F matrix row to all-zero, simulating a
// never-learned verb whose reference-F seed (s.refF) is left intact. NewState
// folds the seed into both F and refF; the exploration floor keys "never
// learned" off the live F-row summing to zero, so a test that needs that state
// must zero F explicitly while keeping refF as the seed of record.
func zeroLearnedFRow(s *State, verb string) {
	for _, ax := range Axes {
		s.F[verb][ax] = 0
	}
}

// TestPickExplorationFloorSkipsOverPacedNeverLearnedVerb is the regression test
// for the exploration-floor over-paced-axis gate. A never-learned storage-only
// verb whose gradient was deliberately zeroed because storage is over-paced
// (residual <= 0) must STAY zeroed — the floor must not resurrect it.
//
// accountverb is left learned (non-zero live F-row) so the projected gradient
// pushes all simplex weight onto it and zeroes storagespammer; only the
// exploration floor could lift storagespammer back, and the gate must stop it.
func TestPickExplorationFloorSkipsOverPacedNeverLearnedVerb(t *testing.T) {
	verbs := []string{"accountverb", "storagespammer"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountverb":    {"accounts": 160, "storage": 0, "code": 0},
			"storagespammer": {"accounts": 0, "storage": 191, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountverb": 1500, "storagespammer": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.5) // ε > 0 so the floor is active
	// storagespammer is never-learned (live F-row all-zero, refF seed retained);
	// accountverb keeps its learned seed so the gradient has something to push.
	zeroLearnedFRow(s, "storagespammer")

	// accounts far under-target, storage far over-target — storage residual <= 0.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	// Over many batch IDs the ε-greedy branch must NEVER reach storagespammer:
	// a zeroed selection weight is unreachable in selectVerbIndex, and the gate
	// must keep the over-paced never-learned verb at zero.
	for batchID := uint64(0); batchID < 300; batchID++ {
		s.BatchID = batchID
		plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
		if plan.Verb == "storagespammer" {
			t.Fatalf("batch %d: exploration floor resurrected an over-paced "+
				"never-learned verb — selected=%q, want accountverb", batchID, plan.Verb)
		}
	}
}

// TestPickExplorationFloorLiftsUnderPacedNeverLearnedVerb verifies the gate is
// not a blanket suppression: the SAME never-learned storage verb IS floored
// when storage is under-paced (residual > 0), so the ε-greedy branch can still
// sample it and learn its F-row.
func TestPickExplorationFloorLiftsUnderPacedNeverLearnedVerb(t *testing.T) {
	verbs := []string{"accountverb", "storagespammer"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountverb":    {"accounts": 160, "storage": 0, "code": 0},
			"storagespammer": {"accounts": 0, "storage": 191, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountverb": 1500, "storagespammer": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.5)
	zeroLearnedFRow(s, "accountverb")
	zeroLearnedFRow(s, "storagespammer")

	// Both axes under-target (zero observation, positive byte targets) → both
	// residuals > 0. The storage verb must be lifted to the epsilon floor.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := zeroObs()

	// Drive Pick directly is opaque (selectFrom is internal); assert via the
	// same selectFrom path Pick uses. With both verbs floored to ε/n and ε>0,
	// the ε-greedy branch can sample either; over many batch IDs the storage
	// verb must be reachable. A zeroed verb is unreachable in selectVerbIndex,
	// so any selection of it proves the floor lifted it.
	storageSelected := false
	for batchID := uint64(0); batchID < 200; batchID++ {
		s.BatchID = batchID
		plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
		if plan.Verb == "storagespammer" {
			storageSelected = true
			break
		}
	}
	if !storageSelected {
		t.Fatalf("under-paced never-learned storage verb was never selected — "+
			"exploration floor failed to lift it (cold-start escape broken)")
	}
}

// TestPickExplorationFloorPreservesColdStartEscape verifies the gate does not
// break the original cold-start-trap escape: a never-learned verb whose seed
// effect lands on an under-paced axis is still floored and reachable.
func TestPickExplorationFloorPreservesColdStartEscape(t *testing.T) {
	// codeverb is the sole grower of the code axis and is never-learned.
	verbs := []string{"accountverb", "codeverb"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountverb": {"accounts": 160, "storage": 0, "code": 0},
			"codeverb":    {"accounts": 0, "storage": 0, "code": 2200},
		},
		AvgTxRLP: map[string]float64{"accountverb": 1500, "codeverb": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity, 0.5)
	zeroLearnedFRow(s, "accountverb")
	zeroLearnedFRow(s, "codeverb")

	// code axis under-target (zero code observed, positive code share) — the
	// never-learned codeverb must escape the cold-start trap and be selectable.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.0, AxisCode: 0.5},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := zeroObs()

	codeSelected := false
	for batchID := uint64(0); batchID < 200; batchID++ {
		s.BatchID = batchID
		plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
		if plan.Verb == "codeverb" {
			codeSelected = true
			break
		}
	}
	if !codeSelected {
		t.Fatalf("cold-start escape broken: never-learned under-paced codeverb "+
			"was never selected — the exploration floor must still lift it")
	}
}

// TestFloorHelpsUnderPacedAxis unit-tests the gate predicate directly.
func TestFloorHelpsUnderPacedAxis(t *testing.T) {
	cases := []struct {
		name     string
		refRow   [3]float64
		residual [3]float64
		want     bool
	}{
		{"storage verb, storage over-paced", [3]float64{0, 191, 0}, [3]float64{100, -50, 0}, false},
		{"storage verb, storage under-paced", [3]float64{0, 191, 0}, [3]float64{-10, 50, 0}, true},
		{"multi-axis verb, one axis under-paced", [3]float64{160, 10, 0}, [3]float64{50, -10, 0}, true},
		{"verb grows only over-paced axes", [3]float64{160, 10, 0}, [3]float64{-1, -1, 5}, false},
		{"all-zero seed row never helps", [3]float64{0, 0, 0}, [3]float64{5, 5, 5}, false},
		{"residual exactly zero is not under-paced", [3]float64{0, 191, 0}, [3]float64{0, 0, 0}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := floorHelpsUnderPacedAxis(tc.refRow, tc.residual); got != tc.want {
				t.Errorf("floorHelpsUnderPacedAxis(%v, %v) = %v, want %v",
					tc.refRow, tc.residual, got, tc.want)
			}
		})
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
