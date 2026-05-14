package controller

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"strconv"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
)

const projectionEta = 0.1
const toleranceFloor = 0.005

// gasCapFraction is the fraction of the block gas limit Pick will plan up to.
// Mirrors the dispatcher's defensive ceiling (0.95 × blockGasLimit).
const gasCapFraction = 0.95

// gasCapSafetyMargin inflates the per-verb base gas estimate before computing
// the upper bound on tx count, so transient over-estimates by the worker still
// fit under the dispatcher's hard 0.95 ceiling.
const gasCapSafetyMargin = 1.50

// baseGasPerVerb is the per-tx gas budget used by Pick when sizing batches.
// Values are empirical upper-bounds observed in bloatnet journals; the safety
// margin (gasCapSafetyMargin) is multiplied on top in computeGasBasedMax. Some
// verbs (storagespam, gasburnertx) genuinely consume 1.5-2M+ gas per tx, so
// under-estimating crashes the dispatcher's hard 0.95 × block-gas assertion.
var baseGasPerVerb = map[string]uint64{
	"eoatx":           21_000,
	"deploytx":        200_000,
	"factorydeploytx": 200_000,
	"storagespam":     2_500_000, // empirical: 2.0M/tx observed
	"storagerefundtx": 100_000,
	"erc20tx":         100_000,
	"erc20_bloater":   1_000_000,
	"uniswap_swaps":   300_000,
	"gasburnertx":     1_500_000, // empirical: 1.5M/tx observed
	"calltx":          100_000,
	"evm_fuzz":        1_000_000,
	"noop":            21_000,
}

// defaultBaseGasPerVerb is used when a verb is missing from baseGasPerVerb.
// Conservative high value so unknown verbs do not blow the gas cap.
const defaultBaseGasPerVerb uint64 = 1_000_000

// computeGasBasedMax returns the maximum number of txs of `verb` that fit
// under `gasCapFraction × blockGasLimit` with a `gasCapSafetyMargin` safety
// factor. Returns math.MaxInt when blockGasLimit is zero (no cap configured).
func computeGasBasedMax(verb string, blockGasLimit uint64) int {
	if blockGasLimit == 0 {
		return math.MaxInt32
	}
	perTx, ok := baseGasPerVerb[verb]
	if !ok {
		perTx = defaultBaseGasPerVerb
	}
	if perTx == 0 {
		return math.MaxInt32
	}
	ceiling := float64(blockGasLimit) * gasCapFraction
	allowed := ceiling / (float64(perTx) * gasCapSafetyMargin)
	if allowed < 1 {
		return 1
	}
	if allowed > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	return int(allowed)
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

	// Gas-awareness: bias the simplex weights toward verbs with the highest
	// bytes-per-gas. Without this, the controller burns master-signer ETH on
	// expensive low-yield verbs (e.g. gasburnertx: 1.5M gas/tx, 0 useful bytes;
	// storagespam: 2.5M gas/tx, modest bytes). The bias is multiplicative and
	// re-normalised onto the simplex, so the gradient direction is preserved
	// while the gas-inefficient tail is squashed. Controlled by
	// $ORCH_GAS_AWARE_EXPONENT (default 0.5 = sqrt bias; 0 = disabled;
	// 2 = aggressive).
	xProj = applyGasAwareBias(verbs, xProj, s)

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
	gasBasedMax := computeGasBasedMax(topVerb, blockGasLimit)
	if gasBasedMax > nMaxHardCeil {
		gasBasedMax = nMaxHardCeil
	}
	if !math.IsInf(capN, 1) {
		raw := int(math.Min(capN, float64(nMaxHardCeil)))
		nMax = clampMax(raw, byteBasedMax)
	} else {
		// Uncapped axis: bound by bytes only so the worker slice stays sane.
		nMax = byteBasedMax
	}
	// Apply the gas-budget cap last. The dispatcher enforces a hard
	// 0.95 × blockGasLimit ceiling; sizing Pick's output to satisfy that here
	// avoids dispatch-time failures and the nonce-gap fallout they trigger
	// when planners run in parallel.
	if gasBasedMax < nMax {
		nMax = gasBasedMax
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

// defaultGasAwareExponent is the bytes-per-gas bias exponent applied to the
// simplex weights in Pick. 0.5 = square-root bias (moderate); 0 disables;
// higher values (e.g. 2) aggressively concentrate on the most gas-efficient
// verbs. Overridden by $ORCH_GAS_AWARE_EXPONENT at runtime.
const defaultGasAwareExponent = 0.5

// gasAwareExponent reads $ORCH_GAS_AWARE_EXPONENT (float). Returns
// defaultGasAwareExponent on missing/invalid input. Negative values are clamped
// to 0 (disable bias) so misconfiguration never flips the bias sign.
func gasAwareExponent() float64 {
	raw := os.Getenv("ORCH_GAS_AWARE_EXPONENT")
	if raw == "" {
		return defaultGasAwareExponent
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return defaultGasAwareExponent
	}
	if v < 0 {
		return 0
	}
	return v
}

// applyGasAwareBias re-weights `xProj` by efficiency[verb] ^ exp where
// efficiency = (sum_axes F[verb][ax]) / baseGas[verb]. Verbs with zero
// efficiency (e.g. gasburnertx with sum_F = 0) get an effective weight of
// (1 / hugeGas)^exp → near zero, which is what we want: don't pick them
// unless they're the only feasible choice (the downstream `selectFrom`
// no-feasible fallback path still handles that edge).
//
// noop is special-cased: it is the pure-idle verb with sum_F = 0 by design;
// we leave its xProj weight untouched so the existing argmax behaviour for
// "nothing left to do" pipelines does not change.
//
// The returned slice is a new allocation; the caller's xProj is not mutated.
// If exp == 0 (disable), returns a copy of xProj unchanged.
func applyGasAwareBias(verbs []string, xProj []float64, s *State) []float64 {
	out := make([]float64, len(xProj))
	copy(out, xProj)

	exp := gasAwareExponent()
	if exp == 0 {
		return out
	}

	weights := make([]float64, len(verbs))
	for i, v := range verbs {
		if v == "noop" {
			// Preserve original weight: noop is the no-op fallback.
			weights[i] = out[i]
			continue
		}
		// Per-tx bytes estimate: sum of F over axes (F is the per-verb-per-axis
		// bytes-per-tx coefficient maintained by Apply).
		bytesPerTx := 0.0
		if row, ok := s.F[v]; ok {
			for _, ax := range Axes {
				bytesPerTx += row[ax]
			}
		}
		if bytesPerTx < 0 {
			bytesPerTx = 0
		}
		gas, ok := baseGasPerVerb[v]
		if !ok {
			gas = defaultBaseGasPerVerb
		}
		if gas == 0 {
			gas = defaultBaseGasPerVerb
		}
		eff := bytesPerTx / float64(gas)
		// Floor to avoid 0^x edge cases (0^0 = 1 in Go's math.Pow); zero-byte
		// verbs (e.g. gasburnertx) should be squashed near-zero, not promoted.
		// We use 1e-18 → eff^0.5 ≈ 1e-9, which after re-normalisation drops the
		// verb's mix share well below 1%.
		if eff <= 0 {
			eff = 1e-18
		}
		bias := math.Pow(eff, exp)
		if math.IsNaN(bias) || math.IsInf(bias, 0) {
			bias = 0
		}
		weights[i] = out[i] * bias
	}

	// Re-normalise to sum to 1. If the bias zeroed everything out (e.g. only
	// gasburnertx is feasible and exp is huge), fall back to the original
	// xProj so we still produce a usable mix.
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
