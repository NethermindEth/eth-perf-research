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
	rf := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{"gasburnertx": {"accounts": 100, "storage": 100, "code": 100}},
		AvgTxRLP: map[string]float64{"gasburnertx": 1500.0},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

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
// ≥85% of the block gas limit while never exceeding the 0.95 hard ceiling.
func TestPickGasPrimarySizerTargetsBlockFill(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000

	verbs := []string{"storagespam"}
	rf := &referencef.ReferenceF{
		Verbs:    map[string]map[string]float64{"storagespam": {"accounts": 100, "storage": 100, "code": 100}},
		AvgTxRLP: map[string]float64{"storagespam": 1500.0},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	tgt := makeTarget(10 * 1024 * 1024 * 1024)
	plan := s.Pick(zeroObs(), tgt, 8*1024*1024, blockGasLimit)
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
		t.Fatalf("under-filled: NMaxTxs=%d * %d = %d gas < 0.85 * %d = %d",
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
	s := newTestState(verbs, rf, identity)

	plan := s.Pick(zeroObs(), makeTarget(10_000_000), 4_000_000, 0)
	if plan == nil {
		t.Fatalf("Pick returned nil plan")
	}
	if plan.NMaxTxs <= 0 {
		t.Fatalf("NMaxTxs = %d, want > 0 (zero gas limit should not cap)", plan.NMaxTxs)
	}
}

// TestPickArgmaxSelectsUnderServedAxisVerb: the deterministic score is
// F[v]·r'. When one axis is OVER its progress-scaled target and another is
// UNDER, the verb that grows the under-served axis must score highest and be
// selected — never the verb growing the over-served axis.
func TestPickArgmaxSelectsUnderServedAxisVerb(t *testing.T) {
	verbs := []string{"accountfiller", "storagefiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountfiller": {"accounts": 1500, "storage": 0, "code": 0},
			"storagefiller": {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountfiller": 1500, "storagefiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// accounts far UNDER target, storage far OVER target.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "accountfiller" {
		t.Fatalf("verb = %q, want accountfiller (under-served axis must win)", plan.Verb)
	}
	if plan.Mix["accountfiller"] <= plan.Mix["storagefiller"] {
		t.Errorf("score must favour the under-served axis verb: "+
			"accountfiller score=%.6g must exceed storagefiller score=%.6g",
			plan.Mix["accountfiller"], plan.Mix["storagefiller"])
	}
}

// TestPickOverPacedAxisPenalizesVerbsThatTouchIt: an over-paced axis carries a
// negative residual, so the dot-product term for any verb that grows that axis
// is negative — penalising it. A verb whose growth lands entirely on the
// over-paced axis must score below a verb that grows an under-paced axis.
func TestPickOverPacedAxisPenalizesVerbsThatTouchIt(t *testing.T) {
	// storageheavy mirrors the live uniswap_swaps seed [8,220,0]: a small
	// accounts coefficient and a large storage one. accountverb only
	// meaningfully grows the under-paced accounts axis.
	verbs := []string{"accountverb", "storageheavy"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountverb":  {"accounts": 160, "storage": 0, "code": 0},
			"storageheavy": {"accounts": 8, "storage": 220, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountverb": 1500, "storageheavy": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// accounts far under-paced, storage far over-paced.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1_000_000, StorageTrieBytes: 500_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	// storageheavy's big storage coefficient is dotted with a large NEGATIVE
	// storage residual → strongly negative score; accountverb scores positive.
	if plan.Mix["storageheavy"] >= plan.Mix["accountverb"] {
		t.Errorf("over-paced storage axis must penalise storageheavy: "+
			"storageheavy score=%.6g >= accountverb score=%.6g",
			plan.Mix["storageheavy"], plan.Mix["accountverb"])
	}
	if plan.Verb != "accountverb" {
		t.Errorf("selected verb=%q, want accountverb (over-paced axis penalty)", plan.Verb)
	}
}

// TestPickRewardsVerbGrowingTwoUnderPacedAxes: the dot product weighs all three
// axes at once, so a verb that grows two under-paced axes accumulates score
// from both terms and must out-score a verb that grows only one of them.
func TestPickRewardsVerbGrowingTwoUnderPacedAxes(t *testing.T) {
	// deploytx grows code AND accounts; accountonly grows only accounts.
	verbs := []string{"accountonly", "deploytx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountonly": {"accounts": 200, "storage": 0, "code": 0},
			"deploytx":    {"accounts": 200, "storage": 0, "code": 3500},
		},
		AvgTxRLP: map[string]float64{"accountonly": 1500, "deploytx": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// accounts and code both under-paced. A non-zero observation makes progress
	// (and therefore the residual) non-zero: storage sits at its on-pace line
	// while accounts and code lag, so their residuals are positive and equal.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.4, AxisStorage: 0.2, AxisCode: 0.4},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{StorageTrieBytes: 400_000_000}
	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "deploytx" {
		t.Fatalf("verb = %q, want deploytx (grows two under-paced axes)", plan.Verb)
	}
	if plan.Mix["deploytx"] <= plan.Mix["accountonly"] {
		t.Errorf("two-axis grower must score higher: deploytx=%.6g accountonly=%.6g",
			plan.Mix["deploytx"], plan.Mix["accountonly"])
	}
}

// TestPickNearNoOpVerbNeverWins: a verb with a near-zero F-row contributes a
// near-zero dot product and must never out-score a real grower of an
// under-paced axis, regardless of how the residual is shaped.
func TestPickNearNoOpVerbNeverWins(t *testing.T) {
	verbs := []string{"nearnoop", "realgrower"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"nearnoop":   {"accounts": 0.001, "storage": 0.001, "code": 0.001},
			"realgrower": {"accounts": 1500, "storage": 0, "code": 0},
		},
		AvgTxRLP: map[string]float64{"nearnoop": 1500, "realgrower": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// accounts is under-paced (positive residual): cum is non-zero from
	// off-target storage growth while accounts lags its share line. The score
	// is then a meaningful signal — realgrower's large accounts F dwarfs
	// nearnoop's near-zero F-row.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{StorageTrieBytes: 400_000_000}
	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "realgrower" {
		t.Fatalf("verb = %q, want realgrower (near-no-op verb must never win)", plan.Verb)
	}
}

// TestPickShrinkageVerbScoresHighOnOverPacedAxis: a negative-F (shrinkage) verb
// on an over-paced axis dots negative-F with a negative residual → a positive
// term. It must score high exactly when shrinkage is called for.
func TestPickShrinkageVerbScoresHighOnOverPacedAxis(t *testing.T) {
	// storagerefundtx shrinks storage (F_storage < 0); accountfiller grows
	// accounts.
	verbs := []string{"accountfiller", "storagerefundtx"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"accountfiller":   {"accounts": 10, "storage": 0, "code": 0},
			"storagerefundtx": {"accounts": 0, "storage": -180, "code": 0},
		},
		AvgTxRLP: map[string]float64{"accountfiller": 1500, "storagerefundtx": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// accounts on-pace, storage massively over-paced (negative residual).
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 1, StorageTrieBytes: 500_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "storagerefundtx" {
		t.Fatalf("verb = %q, want storagerefundtx (shrinkage wanted on over-paced storage)", plan.Verb)
	}
	if plan.Mix["storagerefundtx"] <= 0 {
		t.Errorf("shrinkage verb score=%.6g, want positive (negative-F · negative-r')",
			plan.Mix["storagerefundtx"])
	}
}

// TestPickDeterministic: with identical inputs the picker always returns the
// same verb. The picker has no RNG path — selection is a pure argmax.
func TestPickDeterministic(t *testing.T) {
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
	s := newTestState(verbs, rf, identity)

	tgt := makeTarget(10_000_000)
	obs := zeroObs()

	first := s.Pick(obs, tgt, 4_000_000, 0).Verb
	for i := 0; i < 25; i++ {
		// Advancing BatchID must not perturb the choice — there is no
		// BatchID-seeded RNG in the new picker.
		s.BatchID = uint64(i)
		got := s.Pick(obs, tgt, 4_000_000, 0).Verb
		if got != first {
			t.Fatalf("non-deterministic: got %s on iteration %d, want %s", got, i, first)
		}
	}
}

// TestPickExcludesContractIneligibleVerb: a verb with the most attractive score
// must NOT be selected when its contract dependency is undeployed. uniswap_swaps
// has a storage-heavy F-row that would otherwise win the argmax.
func TestPickExcludesContractIneligibleVerb(t *testing.T) {
	verbs := []string{"eoatx", "uniswap_swaps"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":         {"accounts": 8, "storage": 5, "code": 0},
			"uniswap_swaps": {"accounts": 8, "storage": 220, "code": 0},
		},
		AvgTxRLP: map[string]float64{"eoatx": 1500, "uniswap_swaps": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// Only eoatx is contract-eligible; uniswap_swaps' router is not deployed.
	s.SetEligibleVerbs([]string{"eoatx"})

	// storage under-paced so uniswap_swaps would otherwise be the argmax.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.1, AxisStorage: 0.9, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 400_000_000}
	for i := 0; i < 20; i++ {
		s.BatchID = uint64(i)
		plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
		if plan.Verb != "eoatx" {
			t.Fatalf("batch %d: verb = %q, want eoatx (ineligible verb must never be selected)", i, plan.Verb)
		}
	}
}

// TestPickSelectsVerbOnceDependencyDeployed: the eligibility gate is not a
// permanent ban — the same verb becomes selectable once its contract is marked
// deployed.
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
	s := newTestState(verbs, rf, identity)

	// storage under-paced (positive residual): cum is non-zero from off-target
	// accounts growth while storage lags its share line, so uniswap_swaps'
	// storage-heavy F-row gives it the top score.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.1, AxisStorage: 0.9, AxisCode: 0.0},
		TotalBytes: 10 * 1024 * 1024 * 1024,
	}
	obs := &Observation{AccountTrieBytes: 400_000_000}
	s.SetEligibleVerbs([]string{"eoatx", "uniswap_swaps"})
	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "uniswap_swaps" {
		t.Fatalf("verb = %q, want uniswap_swaps once its dependency is deployed", plan.Verb)
	}
}

// TestPickExcludesVerbWithSubUnitCap: a verb whose per-axis trajectory cap is
// below 1 tx cannot emit a batch and must be excluded from the argmax even when
// its score is highest. The feasible verb is selected instead.
func TestPickExcludesVerbWithSubUnitCap(t *testing.T) {
	// hugestorage has an enormous storage F-row; with storage already at its
	// full target the per-axis cap drives its max_n_txs below 1.
	verbs := []string{"smallaccount", "hugestorage"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"smallaccount": {"accounts": 50, "storage": 0, "code": 0},
			"hugestorage":  {"accounts": 0, "storage": 1e9, "code": 0},
		},
		AvgTxRLP: map[string]float64{"smallaccount": 1500, "hugestorage": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// storage at full target already — any storage growth over-serves it; the
	// huge F makes hugestorage's excess so large its cap < 1.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 1_000_000,
	}
	obs := &Observation{StorageTrieBytes: 500_000}

	plan := s.Pick(obs, tgt, 1024, 8_000_000_000)
	if plan.Verb != "smallaccount" {
		t.Fatalf("verb = %q, want smallaccount (sub-unit-cap verb must be excluded)", plan.Verb)
	}
}

// TestPickEndgameClipNeutralizesOverServedAxis: once the cumulative budget is
// met but an axis is still under-served, a negative residual on a satisfied
// axis is clipped to zero so it neither rewards nor penalises. A verb that
// grows the still-under-served axis must win.
func TestPickEndgameClipNeutralizesOverServedAxis(t *testing.T) {
	verbs := []string{"codefiller", "storagefiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"codefiller":    {"accounts": 0, "storage": 0, "code": 1500},
			"storagefiller": {"accounts": 0, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"codefiller": 1500, "storagefiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// Cumulative budget met (storage hugely over its share) but code still
	// under its target — endgame. The clip zeroes the negative storage
	// residual so codefiller, the only verb serving the under-served code
	// axis, wins.
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.0, AxisStorage: 0.5, AxisCode: 0.5},
		TotalBytes: 1_000_000_000,
	}
	obs := &Observation{StorageTrieBytes: 1_000_000_000, CodeBytesTotal: 1_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "codefiller" {
		t.Fatalf("verb = %q, want codefiller (endgame: under-served code axis must win)", plan.Verb)
	}
}

// TestPickClipsOverServedAxisMidRun is the regression test for the verb-
// fixation stall: mid-run (cum < TotalBytes, so NOT endgame), with storage
// over-paced and accounts under-paced, the unclipped negative storage residual
// used to cancel out a productive verb's positive accounts score, handing the
// argmax to a do-nothing verb (F-row ~= 0) and freezing state growth. The
// unconditional clip must zero the negative residual so the productive verb,
// which serves the under-paced accounts axis, wins.
func TestPickClipsOverServedAxisMidRun(t *testing.T) {
	// "donothing" is index 0: under the old endgame-only clip it would win the
	// score==0 tie against a residual-cancelled productive verb.
	verbs := []string{"donothing", "accountfiller"}
	rf := &referencef.ReferenceF{
		Verbs: map[string]map[string]float64{
			"donothing":     {"accounts": 0, "storage": 0, "code": 0},
			"accountfiller": {"accounts": 1500, "storage": 1500, "code": 0},
		},
		AvgTxRLP: map[string]float64{"donothing": 1500, "accountfiller": 1500},
	}
	var identity [32]byte
	s := newTestState(verbs, rf, identity)

	// cum = 400e6 < TotalBytes 1e9 -> endgame is false. Storage sits at 400e6
	// (over its 0.4-progress 200e6 share); accounts at 0 (under its share).
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.5, AxisStorage: 0.5, AxisCode: 0.0},
		TotalBytes: 1_000_000_000,
	}
	obs := &Observation{AccountTrieBytes: 0, StorageTrieBytes: 400_000_000}

	plan := s.Pick(obs, tgt, 8*1024*1024, 8_000_000_000)
	if plan.Verb != "accountfiller" {
		t.Fatalf("verb = %q, want accountfiller (mid-run over-served storage must not stall on do-nothing verb)", plan.Verb)
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
	s := newTestState(verbs, ref, identity)

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
// mis-learned a gas value far BELOW its true cost must still be sized by the
// static baseline. gasPerTxEstimate returns max(EWMA, baseline), so the
// under-estimate can never oversize the batch.
func TestComputeGasBasedMaxIgnoresMislearnedLowEWMA(t *testing.T) {
	const blockGasLimit uint64 = 8_000_000_000
	const verb = "storagespam" // baseline ~2.5M gas/tx

	verbs := []string{verb}
	ref := makeRef(verbs, 10.0)
	var identity [32]byte
	s := newTestState(verbs, ref, identity)

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

	if est := s.gasPerTxEstimate(verb); est < float64(s.baselineGasPerVerb(verb)) {
		t.Errorf("gasPerTxEstimate=%.0f below baseline=%d — under-estimate not clamped",
			est, s.baselineGasPerVerb(verb))
	}

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
