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

// Pick computes the next batch plan from the current observation and target.
// totalBatchBytes is the hard byte cap for deadline_bytes.
func (s *State) Pick(obs *Observation, tgt *Target, totalBatchBytes int) *BatchPlan {
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

	// nMax: use the axis-headroom cap when finite; otherwise derive from
	// deadlineBytes / avgTxRLP so the worker is never asked to allocate an
	// unreasonably large slice.  A hard ceiling of 64 k txs per batch is
	// also applied as a safety net.
	const nMaxHardCeil = 65536
	nMax := 0
	byteBasedMax := nMaxHardCeil
	if avg > 0 && deadlineBytes > 0 {
		byteBasedMax = clampMin(deadlineBytes/int(avg), 1)
		if byteBasedMax > nMaxHardCeil {
			byteBasedMax = nMaxHardCeil
		}
	}
	if !math.IsInf(capN, 1) {
		raw := int(math.Min(capN, float64(nMaxHardCeil)))
		nMax = clampMax(raw, byteBasedMax)
	} else {
		// Uncapped axis: bound by bytes only so the worker slice stays sane.
		nMax = byteBasedMax
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
