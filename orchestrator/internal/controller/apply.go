package controller

import (
	"fmt"
	"math"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mathx"
)

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

	// Update AvgTxRLP EWMA. The new/old split is config-driven: the prior keeps
	// (1 - AvgTxRLPDecayNew).
	if dispatchedRLPBytes > 0 {
		observedAvg := float64(dispatchedRLPBytes) / float64(txCount)
		prev, ok := s.AvgTxRLP[plan.Verb]
		if !ok {
			prev = observedAvg
		}
		decayNew := s.cfg.Control.AvgTxRLPDecayNew
		s.AvgTxRLP[plan.Verb] = decayNew*observedAvg + (1.0-decayNew)*prev
	}

	// Per-axis observed delta, normalised by tx count.
	//
	// The deltas are signed: post and pre are uint64 trie-byte counters, and a
	// post < pre case (trie compaction, a reorg between the two observations,
	// or stale/out-of-order counters) underflows the uint64 subtraction to a
	// value near 2^64. Computed in int64/float64 the difference stays signed
	// and small; the underflow path is what historically fed ~6.5e13 bytes/tx
	// garbage into UpdateCoeff and diverged F.
	txCountF := float64(txCount)
	observed := [3]float64{
		signedDelta(post.AccountTrieBytes, pre.AccountTrieBytes) / txCountF,
		signedDelta(post.StorageTrieBytes, pre.StorageTrieBytes) / txCountF,
		signedDelta(post.CodeBytesTotal, pre.CodeBytesTotal) / txCountF,
	}

	verb := plan.Verb
	tuning := s.alphaTuning()
	coeffBound := s.cfg.Control.CoeffBound
	sigmaFloor := s.cfg.Control.SigmaFloor

	// Index the verb's row once. A verb absent from the registry is a
	// pre-condition violation by the caller (Pick selects from s.Verbs); guard
	// it defensively rather than panic so a misconfigured test caller doesn't
	// crash the controller.
	rowIdx, known := s.verbIdx[verb]
	if !known {
		return nil, fmt.Errorf("controller: Apply on unknown verb %q", verb)
	}
	row := &s.Rows[rowIdx]

	// Update F, Sigma, Alpha via adaptive-α.
	//
	// Each axis update is guarded so one pathological batch cannot corrupt F:
	//   1. A non-finite or absurd observed per-tx effect (beyond the physical
	//      CoeffBound) is rejected — the axis keeps its current F/σ/α and the
	//      bad observation is dropped rather than folded in.
	//   2. The post-update F and σ are clamped to their physical bounds, so
	//      even an in-range-but-large observation cannot ratchet the matrix
	//      toward the divergent 6.48e13 / 4.15e14 state seen in production.
	// The adaptive-α/Huber math itself (UpdateCoeff) is unchanged.
	for axIdx := 0; axIdx < 3; axIdx++ {
		if !mathx.IsFiniteInRange(observed[axIdx], coeffBound) {
			// Pathological observation — skip this axis's update entirely.
			continue
		}
		result := mathx.UpdateCoeff(
			row.F[axIdx],
			observed[axIdx],
			row.Sigma[axIdx],
			row.Alpha[axIdx],
			tuning,
		)
		// Writes go through atomicStoreFloat so the lock-free snapshot readers
		// (FSnapshot / AlphaSnapshot / SigmaSnapshot) see clean, race-detector
		// safe values. Pick reads plain slots under pickApplyMu — the same mu
		// this Apply call holds — so the writes are still ordered wrt Pick.
		atomicStoreFloat(&row.F[axIdx], mathx.ClampCoeff(result.F, coeffBound))
		atomicStoreFloat(&row.Sigma[axIdx], mathx.ClampSigma(result.Sigma, sigmaFloor, coeffBound))
		atomicStoreFloat(&row.Alpha[axIdx], result.Alpha)
	}

	// Residual: obs_vec - commanded_vec (commanded = F[verb][ax] * txCount after update).
	// Python computes residual BEFORE updating F (uses new F[verb] post-update for commanded).
	// Actually re-reading: Python computes commanded_vec from state.F[verb] which has already
	// been updated in the loop above. We match that.
	commanded := [3]float64{}
	obsVec := [3]float64{}
	for axIdx := 0; axIdx < 3; axIdx++ {
		commanded[axIdx] = row.F[axIdx] * float64(txCount)
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

// signedDelta returns post - pre as a signed float64. post and pre are uint64
// trie-byte counters; computing the difference in int64 keeps a post < pre case
// (trie compaction, a reorg between observations, stale counters) as a small
// negative number instead of underflowing the uint64 subtraction to ~2^64.
func signedDelta(post, pre uint64) float64 {
	return float64(int64(post) - int64(pre))
}

// pushOvershoot records the current batch residual ratio in the rolling window.
// The grace period and trip threshold come from the resolved RunConfig.
func (s *State) pushOvershoot(residualNorm float64, commanded [3]float64) {
	// BatchID has already been incremented above.
	if s.BatchID <= uint64(s.cfg.Control.OvershootGrace) {
		return
	}
	denom := math.Max(l2Norm3(commanded), s.cfg.Control.ResidualNormFloor)
	ratio := residualNorm / denom
	tripped := ratio > s.cfg.Control.OvershootThreshold

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
