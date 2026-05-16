package controller

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
)

const avgTxRLPDecayNew = 0.3
const avgTxRLPDecayOld = 0.7

// Apply updates F, Sigma, Alpha, AvgTxRLP, and VerbStats after a committed
// batch. pre is the observation taken BEFORE the block, post is taken AFTER.
// txCount is the number of txs actually included; dispatchedRLPBytes is their
// total RLP size; gasUsed is the committed block's gas consumption. gasUsed
// may be zero on legacy call paths or before block info is plumbed through;
// VerbStats updates are skipped in that case.
// Returns a ResidualSnapshot and an error if txCount <= 0.
func (s *State) Apply(pre, post *Observation, plan *BatchPlan, txCount int, dispatchedRLPBytes uint64, gasUsed uint64) (*ResidualSnapshot, error) {
	if txCount <= 0 {
		return nil, fmt.Errorf("controller: txCount must be positive, got %d", txCount)
	}

	// Update AvgTxRLP EWMA.
	if dispatchedRLPBytes > 0 {
		observedAvg := float64(dispatchedRLPBytes) / float64(txCount)
		prev, ok := s.AvgTxRLP[plan.Verb]
		if !ok {
			prev = observedAvg
		}
		s.AvgTxRLP[plan.Verb] = avgTxRLPDecayNew*observedAvg + avgTxRLPDecayOld*prev
	}

	// Per-axis observed delta, normalised by tx count.
	observed := [3]float64{
		float64(post.AccountTrieBytes-pre.AccountTrieBytes) / float64(txCount),
		float64(post.StorageTrieBytes-pre.StorageTrieBytes) / float64(txCount),
		float64(post.CodeBytesTotal-pre.CodeBytesTotal) / float64(txCount),
	}

	verb := plan.Verb

	// Update F, Sigma, Alpha via adaptive-α.
	for axIdx, ax := range Axes {
		result := mathx.UpdateCoeff(
			s.F[verb][ax],
			observed[axIdx],
			s.Sigma[verb][ax],
			s.Alpha[verb][ax],
		)
		s.F[verb][ax] = result.F
		s.Sigma[verb][ax] = result.Sigma
		s.Alpha[verb][ax] = result.Alpha
	}

	// Residual: obs_vec - commanded_vec (commanded = F[verb][ax] * txCount after update).
	// Python computes residual BEFORE updating F (uses new F[verb] post-update for commanded).
	// Actually re-reading: Python computes commanded_vec from state.F[verb] which has already
	// been updated in the loop above. We match that.
	commanded := [3]float64{}
	obsVec := [3]float64{}
	for axIdx, ax := range Axes {
		commanded[axIdx] = s.F[verb][ax] * float64(txCount)
		obsVec[axIdx] = observed[axIdx] * float64(txCount)
	}
	diff := [3]float64{obsVec[0] - commanded[0], obsVec[1] - commanded[1], obsVec[2] - commanded[2]}
	residualNorm := l2Norm3(diff)

	s.lastResidualL2 = residualNorm
	s.BatchID++

	// Update overshoot window.
	s.pushOvershoot(residualNorm, commanded)

	// Per-verb gas/bytes EWMA: skipped when gasUsed is zero (legacy call path)
	// or when no RLP bytes were dispatched (bytesPerTx would be undefined).
	if gasUsed > 0 && txCount > 0 {
		bytesPerTx := 0.0
		if dispatchedRLPBytes > 0 {
			bytesPerTx = float64(dispatchedRLPBytes) / float64(txCount)
		}
		s.UpdateVerbStats(verb, gasUsed, uint64(txCount), bytesPerTx)
	}

	perAxis := map[Axis]float64{
		AxisAccounts: diff[0],
		AxisStorage:  diff[1],
		AxisCode:     diff[2],
	}

	return &ResidualSnapshot{
		PerAxis:            perAxis,
		L2Norm:             residualNorm,
		DispatchedRLPBytes: dispatchedRLPBytes,
	}, nil
}

// pushOvershoot records the current batch residual ratio in the rolling window.
// Gated by the grace period (OVERSHOOT_GRACE_BATCHES = 5 in Python).
const overshootGraceBatches = 5
const residualNormFloor = 1024.0

func (s *State) pushOvershoot(residualNorm float64, commanded [3]float64) {
	// BatchID has already been incremented above.
	if s.BatchID <= overshootGraceBatches {
		return
	}
	denom := math.Max(l2Norm3(commanded), residualNormFloor)
	ratio := residualNorm / denom
	threshold := overshootThreshold
	if env := os.Getenv("ORCH_OVERSHOOT_THRESHOLD"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil {
			threshold = v
		}
	}
	tripped := ratio > threshold

	s.overshootMu.Lock()
	defer s.overshootMu.Unlock()
	if s.overshootWindow == nil {
		return
	}
	cap := len(s.overshootWindow)
	if cap == 0 {
		return
	}
	s.overshootWindow[s.overshootHead] = tripped
	s.overshootHead = (s.overshootHead + 1) % cap
	if s.overshootFilled < cap {
		s.overshootFilled++
	}
}

const overshootThreshold = 0.20
