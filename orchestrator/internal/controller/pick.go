package controller

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
)

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
	cum := current.Sum()
	targetTotal := float64(tgt.TotalBytes)
	progress := math.Min(cum/math.Max(targetTotal, 1.0), 1.0)

	// Per-axis full target, desired-at-progress, and residual. Built by axis
	// iteration so the accounts/storage/code order lives only in the Axis enum.
	var desired, residual, targetFull AxisVec
	for a := range numAxes {
		targetFull[a] = tgt.ByteTarget(a)
		desired[a] = progress * targetFull[a]
		residual[a] = desired[a] - current[a]
	}

	// endgame is retained only as a debug/termination signal; the per-term clip
	// in the score loop below is what neutralises over-served axes for scoring.
	endgame := false
	if cum >= targetTotal {
		for a := range numAxes {
			if current[a] < targetFull[a] {
				endgame = true
				break
			}
		}
	}

	fMat := scratch.fMat
	score := scratch.score

	// deficit / weight expose the ratio-on-trajectory scoring inputs for the
	// debug logger. They are populated only when UseRatioScoring is on and
	// the trajectory fallback to the legacy formula is not taken; when the
	// legacy formula runs they stay zero.
	var deficit, ratioWeight AxisVec
	useRatio := s.UseRatioScoring
	ratioFellBack := false
	if useRatio {
		for a := range numAxes {
			expected := tgt.Shares[a] * cum
			if expected > targetFull[a] {
				expected = targetFull[a]
			}
			d := expected - current[a]
			if d < 0 {
				d = 0
			}
			deficit[a] = d
		}
		sumDeficit := deficit.Sum()
		if sumDeficit > 0 {
			for a := range numAxes {
				ratioWeight[a] = deficit[a] / sumDeficit
			}
			for j := range n {
				var sc float64
				for a := range numAxes {
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
		for j := range n {
			var sc float64
			for a := range numAxes {
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

	// Per-axis trajectory cap: max_n_txs[i] = min over the over-serving axes
	// of headroom/excess. Used both to size the batch and to exclude a verb
	// that cannot emit even one tx (cap < 1).
	var p, tolerance AxisVec
	for a := range numAxes {
		p[a] = tgt.Shares[a]
		headroom := math.Max(0, desired[a]-current[a])
		tol := math.Max(p[a], s.cfg.Control.ToleranceFloor) * float64(totalBatchBytes)
		tolerance[a] = headroom + tol
	}
	maxNTxs := scratch.maxNTxs
	for i := range n {
		maxNTxs[i] = math.Inf(1)
	}
	for i := range n {
		fRow := s.Rows[i].F
		fSum := fRow.Sum()
		if fSum <= 0 {
			continue
		}
		capForVerb := math.Inf(1)
		hasOver := false
		for a := range numAxes {
			excess := fRow[a] - p[a]*fSum
			if excess > 0 {
				c := tolerance[a] / excess
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

	topIndex := candidates[0]
	for _, i := range candidates {
		if score[i] > score[topIndex] {
			topIndex = i
		}
	}

	// ε-greedy: with probability Epsilon, explore — replace the greedy verb
	// with a random candidate. Ratio-aware: under ratio-scoring, explore only
	// among verbs that serve a currently-deficient axis (score > 0). A uniform
	// pick would otherwise feed byte-heavy verbs serving an OVER-served axis
	// (e.g. a single random storagespam batch adds more bytes than many account
	// batches), pinning that axis's share up and preventing ratio convergence.
	// Excluding zero-score verbs lets the over-served axis actually fall while
	// still exploring the verbs that matter; it re-enters the pool as soon as
	// its deficit reopens. Falls back to all candidates if the filter is empty.
	explored := false
	if eps := s.cfg.Control.Epsilon; eps > 0 && len(candidates) > 1 && s.rng.Float64() < eps {
		pool := candidates
		if useRatio && !ratioFellBack {
			pool = scratch.explorePool
			for _, i := range candidates {
				if score[i] > 0 {
					pool = append(pool, i)
				}
			}
			scratch.explorePool = pool
			if len(pool) == 0 {
				pool = candidates
			}
		}
		topIndex = pool[s.rng.Intn(len(pool))]
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
		deadlineBytes = max(totalBatchBytes, 1)
	} else {
		safe := int(capN * avg)
		deadlineBytes = max(min(safe, totalBatchBytes), 1)
	}

	// Mix carries the per-verb score, exposed for the Prometheus MixSimplex
	// gauge and the journal record. Allocated fresh (not from scratch) because
	// BatchPlan escapes to the committer goroutine — a reused scratch map would
	// be mutated by the next Pick while the committer still reads it.
	mix := make(map[string]float64, n)
	for i, v := range verbs {
		mix[v] = score[i]
	}

	nMaxHardCeil := s.cfg.Control.NMaxHardCeil
	if perVerb, ok := s.cfg.Control.NMaxHardCeilPerVerb[topVerb]; ok {
		nMaxHardCeil = perVerb
	}
	byteBasedMax := nMaxHardCeil
	if avg > 0 && deadlineBytes > 0 {
		byteBasedMax = min(max(deadlineBytes/int(avg), 1), nMaxHardCeil)
	}
	gasBasedMax := min(s.computeGasBasedMax(topVerb, blockGasLimit), nMaxHardCeil)
	nMax := min(byteBasedMax, gasBasedMax)
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
// called only when the --debug-pick flag is set (State.debugPick). It is purely
// observational — it reads already-computed values and changes no control state.
func (s *State) logPickDebug(
	verbs []string,
	fMat [numAxes][]float64,
	score, maxNTxs []float64,
	residual, deficit, ratioWeight AxisVec,
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

// obsToVec converts an Observation to an AxisVec. This and the Axis enum are the
// only places that define which AxisVec slot holds which observation field.
func obsToVec(obs *Observation) AxisVec {
	return AxisVec{
		AxisAccounts: float64(obs.AccountTrieBytes),
		AxisStorage:  float64(obs.StorageTrieBytes),
		AxisCode:     float64(obs.CodeBytesTotal),
	}
}
