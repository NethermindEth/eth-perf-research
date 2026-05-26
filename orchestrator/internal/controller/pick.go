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

// Pick selects the next batch's verb. Must be called under LockState; Pick
// and Apply share that mutex so Rows reads and writes are coherent.
// Verb score = Σ_a F[v][a] · residual[a] (or deficit-share when ratio-scoring
// is enabled). Greedy argmax with ε-greedy exploration via Control.Epsilon.
// totalBatchBytes caps deadline_bytes; blockGasLimit=0 disables gas capping.
//
// Pick is index-driven: every per-axis or per-verb read is `s.Rows[i].F[ax]`
// or s.pick.<buffer>[i] — no string-keyed map lookup on the hot path, and no
// allocation on a steady-state call. The Mix map IS allocated fresh per call
// because BatchPlan escapes to the committer goroutine; the scratch `mix` on
// State is left ready for the next Pick.
func (s *State) Pick(obs *Observation, tgt *Target, totalBatchBytes int, blockGasLimit uint64) *BatchPlan {
	verbs := s.Verbs
	n := len(verbs)
	scratch := s.pick
	scratch.reset(s.Rows)

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

	// Per-verb score: F-row dotted with the relative-gap residual, with one
	// monotone-bloating rule applied per (verb, axis) term. See historical
	// docstring in apply.go; the scoring math itself is unchanged from the
	// pre-A8 version, only the data source is now position-indexed.
	fMat := scratch.fMat
	score := scratch.score

	// deficit / weight expose the ratio-on-trajectory scoring inputs for the
	// debug logger. They are populated only when UseRatioScoring is on and
	// the trajectory fallback to the legacy formula is not taken; when the
	// legacy formula runs they stay zero.
	var deficit, ratioWeight [3]float64
	useRatio := s.UseRatioScoring
	ratioFellBack := false
	if useRatio {
		targetShares := [3]float64{
			tgt.Shares[AxisAccounts],
			tgt.Shares[AxisStorage],
			tgt.Shares[AxisCode],
		}
		for a := 0; a < 3; a++ {
			expected := targetShares[a] * cum
			if expected > targetFull[a] {
				expected = targetFull[a]
			}
			d := expected - current[a]
			if d < 0 {
				d = 0
			}
			deficit[a] = d
		}
		sumDeficit := deficit[0] + deficit[1] + deficit[2]
		if sumDeficit > 0 {
			for a := 0; a < 3; a++ {
				ratioWeight[a] = deficit[a] / sumDeficit
			}
			for j := 0; j < n; j++ {
				var sc float64
				for a := 0; a < 3; a++ {
					f, w := fMat[a][j], ratioWeight[a]
					if f > 0 && w > 0 {
						sc += f * w
					}
				}
				score[j] = sc
			}
		} else {
			ratioFellBack = true
		}
	}
	if !useRatio || ratioFellBack {
		for j := 0; j < n; j++ {
			var sc float64
			for a := 0; a < 3; a++ {
				f, r := fMat[a][j], residual[a]
				if r < 0 && f > 0 {
					continue
				}
				tgtAxis := targetFull[a]
				if tgtAxis > 0 {
					sc += f * r / tgtAxis
				} else {
					sc += f * r
				}
			}
			score[j] = sc
		}
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
	maxNTxs := scratch.maxNTxs
	for i := 0; i < n; i++ {
		maxNTxs[i] = math.Inf(1)
	}
	for i := 0; i < n; i++ {
		fRow := s.Rows[i].F
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

	// Candidate set: eligible (contract deployed) and feasible (per-axis cap
	// permits >= 1 tx). Relax to feasibility-only, then to all verbs, so the
	// run is never wedged.
	candidates := scratch.candidates
	for i, v := range verbs {
		if s.isContractEligible(v) && maxNTxs[i] >= 1.0 {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		for i := range verbs {
			if maxNTxs[i] >= 1.0 {
				candidates = append(candidates, i)
			}
		}
	}
	if len(candidates) == 0 {
		for i := range verbs {
			candidates = append(candidates, i)
		}
	}
	scratch.candidates = candidates

	// Greedy: argmax score over the candidate set; ties break on lower index.
	topIndex := candidates[0]
	for _, i := range candidates {
		if score[i] > score[topIndex] {
			topIndex = i
		}
	}

	// ε-greedy: with probability Epsilon, explore — replace the greedy verb
	// with a uniformly-random candidate.
	explored := false
	if eps := s.cfg.Control.Epsilon; eps > 0 && len(candidates) > 1 && s.rng.Float64() < eps {
		topIndex = candidates[s.rng.Intn(len(candidates))]
		explored = true
	}
	topVerb := verbs[topIndex]

	if s.debugPick {
		s.logPickDebug(verbs, fMat, score, maxNTxs, residual, deficit, ratioWeight, useRatio, ratioFellBack, endgame, cum, progress, topVerb, explored)
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
	// gauge and the journal record. Allocated fresh because BatchPlan escapes
	// to the committer goroutine; the scratch.mix on State is reserved for
	// internal use only.
	mix := make(map[string]float64, n)
	for i, v := range verbs {
		mix[v] = score[i]
	}

	// nMax is the MINIMUM of three bounds plus a hard ceiling — see legacy
	// docstring; logic unchanged.
	nMaxHardCeil := s.cfg.Control.NMaxHardCeil
	if perVerb, ok := s.cfg.Control.NMaxHardCeilPerVerb[topVerb]; ok {
		nMaxHardCeil = perVerb
	}
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
	residual, deficit, ratioWeight [3]float64,
	useRatio, ratioFellBack bool,
	endgame bool,
	cum, progress float64,
	selectedVerb string,
	explored bool,
) {
	var sb strings.Builder
	for j, v := range verbs {
		fSum := fMat[0][j] + fMat[1][j] + fMat[2][j]

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
		"deficit_accounts", deficit[0],
		"deficit_storage", deficit[1],
		"deficit_code", deficit[2],
		"weight_accounts", ratioWeight[0],
		"weight_storage", ratioWeight[1],
		"weight_code", ratioWeight[2],
		"use_ratio_scoring", useRatio,
		"ratio_fell_back", ratioFellBack,
		"endgame", endgame,
		"cum", cum,
		"progress", progress,
		"selected_verb", selectedVerb,
		"explored", explored,
		"verbs", sb.String(),
	)
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
