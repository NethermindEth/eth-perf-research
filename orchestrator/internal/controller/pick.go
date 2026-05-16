package controller

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/rand/v2"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
)

const projectionEta = 0.1
const toleranceFloor = 0.005

// gasCapFraction is the dispatcher's hard ceiling (0.95 × blockGasLimit): a
// batch whose cumulative gas exceeds it is rejected at dispatch time.
const gasCapFraction = 0.95

// gasFillFraction is the fraction of the block gas limit Pick targets when
// sizing a batch. It is the PRIMARY batch sizer: NMaxTxs is driven to fill
// ~90% of the block's gas, sitting safely below the dispatcher's 0.95 hard
// ceiling so transient per-tx over-estimates still dispatch cleanly.
const gasFillFraction = 0.90

// computeGasBasedMax returns the number of txs of `verb` that fill
// `gasFillFraction × blockGasLimit`. This is the primary batch-size bound;
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
		perTx = float64(defaultBaseGasPerVerb)
	}
	ceiling := float64(blockGasLimit) * gasFillFraction
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
	hardCeiling := float64(blockGasLimit) * gasCapFraction
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
	estimate := float64(baselineGasPerVerb(verb))
	vs := s.GetVerbStats(verb)
	if vs != nil && vs.Samples >= verbStatsColdStartN {
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

	// Build F matrix: fMat[axIdx][verbIdx].
	fMat := s.buildFMatrix(verbs)

	// Initial uniform x.
	x := make([]float64, n)
	for i := range x {
		x[i] = 1.0 / float64(n)
	}

	// Endgame clip: cum >= target_total and any axis still under-served.
	targetFull := [3]float64{
		tgt.ByteTarget(AxisAccounts),
		tgt.ByteTarget(AxisStorage),
		tgt.ByteTarget(AxisCode),
	}
	endgame := cum >= targetTotal && (current[0] < targetFull[0] || current[1] < targetFull[1] || current[2] < targetFull[2])
	residualForGrad := residual
	if endgame {
		for i := range residualForGrad {
			if residualForGrad[i] < 0 {
				residualForGrad[i] = 0
			}
		}
	}

	// Gradient: 2 * F^T * (F*x - residualForGrad).
	fx := matVecMul3(fMat, x) // shape [3]
	diff := [3]float64{fx[0] - residualForGrad[0], fx[1] - residualForGrad[1], fx[2] - residualForGrad[2]}
	grad := make([]float64, n)
	for j := 0; j < n; j++ {
		for axIdx := 0; axIdx < 3; axIdx++ {
			grad[j] += 2.0 * fMat[axIdx][j] * diff[axIdx]
		}
	}
	gn := l2NormSlice(grad)
	if gn > 1e-12 {
		for i := range grad {
			grad[i] /= gn
		}
	}

	// Projected gradient step onto simplex.
	step := make([]float64, n)
	for i := range step {
		step[i] = x[i] - projectionEta*grad[i]
	}
	xProj := mathx.ProjectSimplex(step)

	// Per-axis shares and tolerance.
	p := [3]float64{
		tgt.Shares[AxisAccounts],
		tgt.Shares[AxisStorage],
		tgt.Shares[AxisCode],
	}
	tolerance := [3]float64{}
	for axIdx := 0; axIdx < 3; axIdx++ {
		headroom := math.Max(0, desired[axIdx]-current[axIdx])
		tol := math.Max(p[axIdx], toleranceFloor) * float64(totalBatchBytes)
		tolerance[axIdx] = headroom + tol
	}

	flow := RatioFlowCap(obs, tgt, totalBatchBytes)
	for axIdx := 0; axIdx < 3; axIdx++ {
		if flow[axIdx] < tolerance[axIdx] {
			tolerance[axIdx] = flow[axIdx]
		}
	}

	// Per-verb cap: max_n_txs[i] = min over over-serving axes of headroom/excess.
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

	// Feasibility filter: zero verbs whose cap < 1 tx.
	selectFrom := make([]float64, n)
	anyFeasible := false
	for i, cap := range maxNTxs {
		if cap >= 1.0 {
			selectFrom[i] = xProj[i]
			anyFeasible = true
		}
	}
	if !anyFeasible {
		copy(selectFrom, xProj)
	}

	// Unlearned-verb exploration floor (cold-start trap escape).
	//
	// WHY: a verb that has never run has an all-zero F-row, so its gradient is
	// 0; the projected-gradient step leaves it at the baseline 1/n while
	// productive verbs get pushed, and ProjectSimplex then clamps the
	// untouched weight to exactly 0. A 0-weight verb is unreachable in
	// selectVerbIndex (both the argmax and the ε-greedy weighted-sample branch
	// skip it), so it can never get its first run — and so its F-row never
	// gets learned: a self-perpetuating dead state.
	//
	// Force any feasible, never-learned verb to at least Epsilon/n here so the
	// ε-greedy branch can sample it, get its first Apply, and populate its
	// F-row. Once learned (F-row non-zero) the floor no longer applies and the
	// verb competes purely on its gradient merit — this protects the
	// exploration branch the design already has without biasing the verb mix.
	if anyFeasible && s.Epsilon > 0 && n > 0 {
		epsilonFloor := s.Epsilon / float64(n)
		for i, v := range verbs {
			if maxNTxs[i] < 1.0 {
				continue // infeasible — skip
			}
			fRow := s.axisVec(v)
			if fRow[0]+fRow[1]+fRow[2] == 0 && selectFrom[i] < epsilonFloor {
				selectFrom[i] = epsilonFloor
			}
		}
	}

	total := sumFloats(selectFrom)
	if total > 0 {
		for i := range selectFrom {
			selectFrom[i] /= total
		}
	}

	topIndex := s.selectVerbIndex(selectFrom)
	topVerb := verbs[topIndex]

	avg := s.AvgTxRLP[topVerb]
	if avg <= 0 {
		avg = defaultAvgTxRLP
	}

	var deadlineBytes int
	capN := maxNTxs[topIndex]
	if math.IsInf(capN, 1) {
		deadlineBytes = clampMin(totalBatchBytes, 1)
	} else {
		safe := int(capN * avg)
		deadlineBytes = clampMin(clampMax(safe, totalBatchBytes), 1)
	}

	mix := make(map[string]float64, n)
	for i, v := range verbs {
		mix[v] = xProj[i]
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
	//      Measured: ~65 k-tx blocks commit at ~47 µs/tx, ~7-12 k-tx blocks at
	//      ~25-30 µs/tx — large blocks are ~1.7x worse per tx, so a smaller cap
	//      raises sustained throughput. 12 k matches the Python reference orch.
	const nMaxHardCeil = 12000
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

// selectVerbIndex implements ε-greedy with deterministic RNG.
// Seed: sha256(chainIdentity || batchID_big_endian).
// Python: seed_input = f"{chain_identity_hash}:{target_sha256}:{batch_id}"
// We encode the same components in binary for determinism.
func (s *State) selectVerbIndex(weights []float64) int {
	if s.Epsilon <= 0 {
		return argmaxFloats(weights)
	}
	tot := sumFloats(weights)
	if tot <= 0 || math.IsInf(tot, 0) || math.IsNaN(tot) {
		return argmaxFloats(weights)
	}

	h := sha256.New()
	h.Write(s.ChainIdentity[:])
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], s.BatchID)
	h.Write(buf[:])
	digest := h.Sum(nil)

	seed0 := binary.BigEndian.Uint64(digest[:8])
	seed1 := binary.BigEndian.Uint64(digest[8:16])
	rng := rand.New(rand.NewPCG(seed0, seed1))

	if rng.Float64() >= s.Epsilon {
		return argmaxFloats(weights)
	}
	// Weighted sample without normalising (weights already sum to ~1).
	r := rng.Float64() * tot
	cum := 0.0
	for i, w := range weights {
		cum += w
		if r <= cum {
			return i
		}
	}
	return len(weights) - 1
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

// matVecMul3 multiplies a [3][n] matrix by an n-vec, returning a [3] result.
func matVecMul3(mat [3][]float64, v []float64) [3]float64 {
	var out [3]float64
	for i := 0; i < 3; i++ {
		for j, x := range v {
			out[i] += mat[i][j] * x
		}
	}
	return out
}

func l2NormSlice(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

func l2Norm3(v [3]float64) float64 {
	return math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
}

func sumFloats(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s
}

func argmaxFloats(v []float64) int {
	best := 0
	for i := 1; i < len(v); i++ {
		if v[i] > v[best] {
			best = i
		}
	}
	return best
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
