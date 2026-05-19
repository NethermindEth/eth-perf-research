package controller

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
)

// The gas-fill / gas-cap fractions and the nMax hard ceiling live in
// config.RunConfig.Control and are read off the State's cfg.
//
// computeGasBasedMax returns the number of txs of `verb` that fill
// `GasFillFraction × blockGasLimit`. This is the primary batch-size bound;
// the byte budget is only a secondary clamp. Returns math.MaxInt32 when
// blockGasLimit is zero (no cap configured — fresh client, first batch).
//
// Sourcing per-tx gas: gasPerTxEstimate returns max(learned EWMA, static
// baselineGasPerVerb). Both inputs are always non-zero, so for a non-zero
// blockGasLimit this bound is always finite and binding — it can never return
// an unbounded value, which is what previously let an over-sized batch crash
// the dispatcher. The max also makes the estimate bias UP: a mis-learned low
// EWMA can never push the cap above what the baseline permits.
func (s *State) computeGasBasedMax(verb string, blockGasLimit uint64) int {
	if blockGasLimit == 0 {
		return math.MaxInt32
	}
	perTx := s.gasPerTxEstimate(verb)
	if perTx <= 0 {
		// gasPerTxEstimate falls back to the (non-zero) baseline table, so this
		// is unreachable; guard defensively against a corrupted estimate rather
		// than returning an unbounded cap for a non-zero gas limit.
		perTx = float64(s.cfg.Control.DefaultBaseGasPerVerb)
	}
	ceiling := float64(blockGasLimit) * s.cfg.Control.GasFillFraction
	allowed := ceiling / perTx
	if allowed < 1 {
		return 1
	}
	if allowed > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	cap := int(allowed)
	// Safety assert: the cap sized against `perTx` (>= baseline) must never let
	// the batch's baseline-priced gas exceed the dispatcher's hard 0.95 ceiling.
	// floor() already guarantees this, but shrink defensively if rounding or a
	// future change ever breaks the invariant.
	hardCeiling := float64(blockGasLimit) * s.cfg.Control.GasCapFraction
	for cap > 1 && float64(cap)*perTx > hardCeiling {
		cap--
	}
	return cap
}

// gasPerTxEstimate returns the per-tx gas estimate used to SIZE batches against
// the dispatcher's hard gas ceiling. It returns max(learned EWMA, static
// baseline).
//
// Why max, not "EWMA when warm, else baseline": this estimate divides the gas
// ceiling to compute the batch tx count, so the risk is asymmetric.
// UNDER-estimating gas is catastrophic — it oversizes the batch, the dispatcher
// rejects the whole oversized batch, and 20 consecutive rejections terminate the
// run. OVER-estimating is harmless — it just makes batches slightly smaller. The
// EWMA can mis-learn a value far below a verb's true cost (polluted by
// partially-included or rejected batches); clamping up to the known-correct
// static baseline guarantees computeGasBasedMax can never oversize a batch past
// what the baseline permits, regardless of EWMA pollution.
func (s *State) gasPerTxEstimate(verb string) float64 {
	estimate := float64(s.baselineGasPerVerb(verb))
	vs := s.GetVerbStats(verb)
	if vs != nil && vs.Samples >= s.cfg.Control.VerbStatsColdStartN {
		if v := vs.GasPerTx.Value(); v > estimate {
			estimate = v
		}
	}
	return estimate
}

// Pick is goroutine-safe for concurrent readers. State fields are written only
// by Apply (commit goroutine); the read-write race on F/Sigma/Alpha is benign
// — readers see either pre-Apply or post-Apply state, both valid plan inputs.
//
// Pick computes the next batch plan from the current observation and target.
// totalBatchBytes is the hard byte cap for deadline_bytes. blockGasLimit is
// the current head-block gas limit (0 = no gas cap applied).
//
// Selection is one deterministic step (design-v4 §3.1): the verb whose learned
// F-row, dotted with the endgame-clipped trajectory residual vector, yields the
// highest score is chosen. score(v) = Σ_a F[v][a]·r'[a] is design-v3's
// gradient direction Fᵀr evaluated once; since dispatch is single-verb-per-batch
// the projected-gradient descent + its re-lift floors are unnecessary scaffolding
// and have been removed. No RNG, no ε-greedy: the picker is fully deterministic.
func (s *State) Pick(obs *Observation, tgt *Target, totalBatchBytes int, blockGasLimit uint64) *BatchPlan {
	verbs := s.Verbs
	n := len(verbs)

	current := obsToVec(obs)
	cum := current[0] + current[1] + current[2]
	targetTotal := float64(tgt.TotalBytes)
	progress := math.Min(cum/math.Max(targetTotal, 1.0), 1.0)

	// desired per-axis at current progress
	desired := [3]float64{
		progress * tgt.ByteTarget(AxisAccounts),
		progress * tgt.ByteTarget(AxisStorage),
		progress * tgt.ByteTarget(AxisCode),
	}
	residual := [3]float64{
		desired[0] - current[0],
		desired[1] - current[1],
		desired[2] - current[2],
	}

	// endgame is retained only as a debug/termination signal; the per-term clip
	// in the score loop below is what neutralises over-served axes for scoring.
	targetFull := [3]float64{
		tgt.ByteTarget(AxisAccounts),
		tgt.ByteTarget(AxisStorage),
		tgt.ByteTarget(AxisCode),
	}
	endgame := cum >= targetTotal && (current[0] < targetFull[0] || current[1] < targetFull[1] || current[2] < targetFull[2])

	// Per-verb score: F-row dotted with the residual, with one monotone-bloating
	// rule applied per (verb, axis) term. On an over-served axis (residual < 0)
	// an additive verb (F > 0) cannot shrink the axis and cannot avoid touching
	// it — penalising it there only hands the argmax to a do-nothing verb and
	// stalls the run, so that term is clipped to zero. A shrinkage verb (F < 0)
	// on an over-served axis keeps its positive (negative·negative) term, since
	// shrinkage is exactly what is wanted there. Under-served axes (residual >=
	// 0) score normally on every verb.
	fMat := s.buildFMatrix(verbs)
	score := make([]float64, n)
	for j := 0; j < n; j++ {
		var sc float64
		for a := 0; a < 3; a++ {
			f, r := fMat[a][j], residual[a]
			if r < 0 && f > 0 {
				continue
			}
			sc += f * r
		}
		score[j] = sc
	}

	// Per-axis trajectory cap (design-v4 §2.6): max_n_txs[i] = min over the
	// over-serving axes of headroom/excess. Used both to size the batch and to
	// exclude a verb that cannot emit even one tx (cap < 1).
	p := [3]float64{
		tgt.Shares[AxisAccounts],
		tgt.Shares[AxisStorage],
		tgt.Shares[AxisCode],
	}
	tolerance := [3]float64{}
	for axIdx := 0; axIdx < 3; axIdx++ {
		headroom := math.Max(0, desired[axIdx]-current[axIdx])
		tol := math.Max(p[axIdx], s.cfg.Control.ToleranceFloor) * float64(totalBatchBytes)
		tolerance[axIdx] = headroom + tol
	}
	maxNTxs := make([]float64, n)
	for i := range maxNTxs {
		maxNTxs[i] = math.Inf(1)
	}
	for i, v := range verbs {
		fRow := s.axisVec(v)
		fSum := fRow[0] + fRow[1] + fRow[2]
		if fSum <= 0 {
			continue
		}
		capForVerb := math.Inf(1)
		hasOver := false
		for axIdx := 0; axIdx < 3; axIdx++ {
			excess := fRow[axIdx] - p[axIdx]*fSum
			if excess > 0 {
				c := tolerance[axIdx] / excess
				if c < capForVerb {
					capForVerb = c
				}
				hasOver = true
			}
		}
		if hasOver {
			maxNTxs[i] = capForVerb
		}
	}

	// argmax over eligible, feasible verbs. A verb is eligible when its contract
	// dependency is deployed (structurally dead verbs are 100%-rejected on
	// commit) and feasible when its per-axis cap permits at least one tx. The
	// first such verb seeds the argmax; ties break on the lower verb index
	// (deterministic). If no verb is both eligible and feasible the eligibility
	// constraint is relaxed to feasibility-only so the run is not wedged; if
	// still none, every verb is a candidate.
	topIndex := -1
	for i, v := range verbs {
		if !s.isContractEligible(v) || maxNTxs[i] < 1.0 {
			continue
		}
		if topIndex < 0 || score[i] > score[topIndex] {
			topIndex = i
		}
	}
	if topIndex < 0 {
		for i := range verbs {
			if maxNTxs[i] < 1.0 {
				continue
			}
			if topIndex < 0 || score[i] > score[topIndex] {
				topIndex = i
			}
		}
	}
	if topIndex < 0 {
		for i := range verbs {
			if topIndex < 0 || score[i] > score[topIndex] {
				topIndex = i
			}
		}
	}
	topVerb := verbs[topIndex]

	if s.debugPick {
		s.logPickDebug(verbs, fMat, score, maxNTxs, residual, endgame, cum, progress, topVerb)
	}

	avg := s.AvgTxRLP[topVerb]
	if avg <= 0 {
		avg = s.cfg.Control.DefaultAvgTxRLP
	}

	var deadlineBytes int
	capN := maxNTxs[topIndex]
	if math.IsInf(capN, 1) {
		deadlineBytes = clampMin(totalBatchBytes, 1)
	} else {
		safe := int(capN * avg)
		deadlineBytes = clampMin(clampMax(safe, totalBatchBytes), 1)
	}

	// Mix carries the per-verb score, exposed for the Prometheus MixSimplex
	// gauge and the journal record. It is observational only — selection is the
	// argmax above, not a sampled mix.
	mix := make(map[string]float64, n)
	for i, v := range verbs {
		mix[v] = score[i]
	}

	// nMax is the MINIMUM of three bounds plus a hard ceiling:
	//
	//   1. gasBasedMax  — PRIMARY. floor(0.90 × blockGasLimit / gasPerTx(verb)).
	//      Drives the batch to ~90% gas fill. Always finite & binding for a
	//      non-zero blockGasLimit; only a zero gas limit (fresh client) leaves
	//      it unbounded so the first batch is not wedged.
	//   2. byteBasedMax — SECONDARY clamp. floor(deadlineBytes / avgTxRLP).
	//      Nethermind rejects blocks above BlockProductionMaxTxKilobytes, so
	//      this stays a real cap; for ultra-cheap verbs (eoatx) it is still the
	//      binding constraint — that is physics, not a bug.
	//   3. axis-headroom cap (capN) — when an over-served axis limits the verb.
	//   4. nMaxHardCeil — caps batch size for Nethermind commit efficiency.
	nMaxHardCeil := s.cfg.Control.NMaxHardCeil
	byteBasedMax := nMaxHardCeil
	if avg > 0 && deadlineBytes > 0 {
		byteBasedMax = clampMin(deadlineBytes/int(avg), 1)
		if byteBasedMax > nMaxHardCeil {
			byteBasedMax = nMaxHardCeil
		}
	}
	gasBasedMax := s.computeGasBasedMax(topVerb, blockGasLimit)
	if gasBasedMax > nMaxHardCeil {
		gasBasedMax = nMaxHardCeil
	}
	nMax := gasBasedMax
	if byteBasedMax < nMax {
		nMax = byteBasedMax
	}
	if !math.IsInf(capN, 1) {
		if raw := int(math.Min(capN, float64(nMaxHardCeil))); raw < nMax {
			nMax = raw
		}
	}
	if nMax < 1 {
		nMax = 1
	}

	return &BatchPlan{
		Verb:          topVerb,
		DeadlineBytes: deadlineBytes,
		Mix:           mix,
		NMaxTxs:       nMax,
	}
}

// logPickDebug emits one structured slog line per batch capturing, for every
// verb, the score and per-verb cap that drive the deterministic argmax. It is
// called only when ORCH_DEBUG_PICK is truthy ($ORCH_DEBUG_PICK gate, read once
// at construction). It is purely observational — it reads already-computed
// values and changes no control state.
func (s *State) logPickDebug(
	verbs []string,
	fMat [3][]float64,
	score, maxNTxs []float64,
	residual [3]float64,
	endgame bool,
	cum, progress float64,
	selectedVerb string,
) {
	var sb strings.Builder
	for j, v := range verbs {
		fSum := fMat[0][j] + fMat[1][j] + fMat[2][j]

		// Classify why (if at all) this verb could not be selected.
		//   contract-ineligible — undeployed contract dependency.
		//   cap-zeroed           — per-verb cap < 1 tx (over-served axis).
		reason := "candidate"
		switch {
		case !s.isContractEligible(v):
			reason = "contract-ineligible"
		case maxNTxs[j] < 1.0:
			reason = "cap-zeroed"
		}

		capStr := "inf"
		if !math.IsInf(maxNTxs[j], 1) {
			capStr = fmt.Sprintf("%.3f", maxNTxs[j])
		}

		if j > 0 {
			sb.WriteByte(' ')
		}
		// verb|F=[acc,sto,code]|fSum|score|cap|reason
		fmt.Fprintf(&sb,
			"{verb=%s F=[%.6g,%.6g,%.6g] fSum=%.6g score=%.6g cap=%s reason=%s}",
			v, fMat[0][j], fMat[1][j], fMat[2][j], fSum,
			score[j], capStr, reason)
	}

	slog.Info("pick debug",
		"batch_id", s.BatchID,
		"residual_accounts", residual[0],
		"residual_storage", residual[1],
		"residual_code", residual[2],
		"endgame", endgame,
		"cum", cum,
		"progress", progress,
		"selected_verb", selectedVerb,
		"verbs", sb.String(),
	)
}

// buildFMatrix builds a [3][n] matrix: rows = axes, cols = verbs.
func (s *State) buildFMatrix(verbs []string) [3][]float64 {
	n := len(verbs)
	var mat [3][]float64
	for i := range mat {
		mat[i] = make([]float64, n)
	}
	for j, v := range verbs {
		for axIdx, ax := range Axes {
			mat[axIdx][j] = s.F[v][ax]
		}
	}
	return mat
}

// axisVec returns the [3]float64 F-row for a verb, ordered by Axes.
func (s *State) axisVec(verb string) [3]float64 {
	var out [3]float64
	for i, ax := range Axes {
		out[i] = s.F[verb][ax]
	}
	return out
}

// obsToVec converts an Observation to an axis-ordered float64 triple.
func obsToVec(obs *Observation) [3]float64 {
	return [3]float64{
		float64(obs.AccountTrieBytes),
		float64(obs.StorageTrieBytes),
		float64(obs.CodeBytesTotal),
	}
}

func l2Norm3(v [3]float64) float64 {
	return math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
}

func clampMin(v, lo int) int {
	if v < lo {
		return lo
	}
	return v
}

func clampMax(v, hi int) int {
	if v > hi {
		return hi
	}
	return v
}
