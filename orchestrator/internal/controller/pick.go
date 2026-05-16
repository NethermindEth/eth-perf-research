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
// Sourcing per-tx gas: gasPerTxEstimate uses the EWMA estimate once VerbStats
// has accumulated verbStatsColdStartN samples, else the static baselineGasPerVerb
// table. Both are always non-zero, so for a non-zero blockGasLimit this bound is
// always finite and binding — it can never return an unbounded value, which is
// what previously let an over-sized cold-start batch crash the dispatcher.
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
	return int(allowed)
}

// gasPerTxEstimate returns the EWMA gas-per-tx when samples >= cold-start
// threshold, else the static baseline.
func (s *State) gasPerTxEstimate(verb string) float64 {
	vs := s.GetVerbStats(verb)
	if vs != nil && vs.Samples >= verbStatsColdStartN {
		if v := vs.GasPerTx.Value(); v > 0 {
			return v
		}
	}
	return float64(baselineGasPerVerb(verb))
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

	// Cheap-bloat re-weight: bias the simplex weights toward verbs delivering
	// the most state-bytes per ETH spent. bytesPerEth = (sum_axes F[verb]) /
	// EthPerTx[verb]; EthPerTx is seeded non-zero from the baseline gas table so
	// the divisor is always positive. Unlearned verbs (F still zero) get a
	// neutral 1.0 multiplier so they keep being explored — this is the explicit
	// anti-deadlock rule that replaces the broken applyGasAwareBias, which gave
	// cold-start verbs an efficiency of 0 and let noop win the mix permanently.
	xProj = applyCheapBloatBias(verbs, xProj, s)

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
	//   4. nMaxHardCeil — a 64 k-tx safety net on the worker slice size.
	const nMaxHardCeil = 65536
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

// applyCheapBloatBias re-weights `xProj` toward verbs that deliver the most
// state-bytes per ETH spent ("keep bloating cheap"). For each verb:
//
//	bytesPerTx  = sum_axes F[verb][ax]   — the learned byte yield
//	ethPerTx    = s.EthPerTx[verb]       — the learned ETH cost (always > 0,
//	                                       seeded from the baseline gas table)
//	bytesPerEth = bytesPerTx / ethPerTx
//
// The per-verb bytesPerEth values are normalised by their mean so the
// multiplier is O(1): above-average verbs are boosted, below-average damped.
// The result is re-projected onto the simplex.
//
// Anti-deadlock rule: a verb whose F is still zero (never observed) gets a
// NEUTRAL multiplier of 1.0 — never zero. This is what keeps unlearned verbs in
// the mix so ε-greedy explores them, F learns, and the bias becomes meaningful.
// It is the explicit fix for the broken applyGasAwareBias, which assigned
// cold-start verbs an efficiency of 0 → ~0 weight → noop won the mix forever.
// If every verb is unlearned (cold start), all multipliers are 1.0 and xProj
// passes through unchanged.
//
// noop is special-cased: it is the pure-idle fallback, so its xProj weight is
// preserved untouched.
//
// The returned slice is a new allocation; the caller's xProj is not mutated.
func applyCheapBloatBias(verbs []string, xProj []float64, s *State) []float64 {
	out := make([]float64, len(xProj))
	copy(out, xProj)

	// bytesPerEth per verb; -1 marks noop (preserve weight, skip normalisation).
	bytesPerEth := make([]float64, len(verbs))
	sum := 0.0
	count := 0
	for i, v := range verbs {
		if v == "noop" {
			bytesPerEth[i] = -1
			continue
		}
		bytesPerTx := 0.0
		if row, ok := s.F[v]; ok {
			for _, ax := range Axes {
				bytesPerTx += row[ax]
			}
		}
		if bytesPerTx <= 0 {
			// Unlearned verb: neutral 1.0 multiplier (anti-deadlock rule).
			bytesPerEth[i] = -1
			continue
		}
		ethPerTx := s.EthPerTx[v]
		if ethPerTx <= 0 || math.IsNaN(ethPerTx) || math.IsInf(ethPerTx, 0) {
			// EthPerTx is seeded > 0; a non-positive value would only arise
			// from a corrupted update — treat as unlearned and stay neutral.
			bytesPerEth[i] = -1
			continue
		}
		bpe := bytesPerTx / ethPerTx
		if math.IsNaN(bpe) || math.IsInf(bpe, 0) {
			bytesPerEth[i] = -1
			continue
		}
		bytesPerEth[i] = bpe
		sum += bpe
		count++
	}

	// No learned verb to normalise against → bias is a no-op (cold start).
	if count == 0 || sum <= 0 {
		return out
	}
	mean := sum / float64(count)

	weights := make([]float64, len(verbs))
	for i := range verbs {
		bpe := bytesPerEth[i]
		if bpe < 0 {
			// noop or unlearned verb: neutral 1.0 multiplier.
			weights[i] = out[i]
			continue
		}
		mult := bpe / mean
		if math.IsNaN(mult) || math.IsInf(mult, 0) || mult < 0 {
			mult = 1.0
		}
		weights[i] = out[i] * mult
	}

	// Re-normalise onto the simplex. If the bias zeroed everything (shouldn't
	// happen — multipliers are >= 0 and at least noop/unlearned verbs keep
	// their weight), fall back to the original xProj.
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return out
	}
	for i := range weights {
		weights[i] /= total
	}
	return weights
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
