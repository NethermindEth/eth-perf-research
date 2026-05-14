package controller

import "math"

const (
	ratioTolerance = 0.02
	floorFraction  = 0.10
)

// RatioFlowCap returns the per-axis byte budget the next batch is allowed to
// emit before the cumulative ratio deviates from target.Shares by more than
// ratioTolerance. Each axis is floored at floorFraction * share *
// totalBatchBytes so the cap never collapses to zero — even a fully-served
// axis keeps some baseline flow.
//
// The cap is intersected with the existing tolerance[] array in Pick (taking
// the min) so over-served axes get throttled harder than the share-based floor
// alone provides.
func RatioFlowCap(obs *Observation, t *Target, totalBatchBytes int) [3]float64 {
	if obs == nil || t == nil || totalBatchBytes <= 0 {
		return [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
	}

	curr := [3]float64{
		float64(obs.AccountTrieBytes),
		float64(obs.StorageTrieBytes),
		float64(obs.CodeBytesTotal),
	}
	currSum := curr[0] + curr[1] + curr[2]
	newTotal := currSum + float64(totalBatchBytes)

	share := [3]float64{
		t.Shares[AxisAccounts],
		t.Shares[AxisStorage],
		t.Shares[AxisCode],
	}

	var out [3]float64
	for i := 0; i < 3; i++ {
		ratioErr := share[i]
		if currSum > 0 {
			ratioErr = share[i] - curr[i]/currSum
		}
		shifted := share[i]
		if ratioErr >= 0 {
			shifted = share[i] + ratioTolerance
		} else {
			shifted = share[i] - ratioTolerance
		}
		if shifted < 0 {
			shifted = 0
		}
		desired := newTotal * shifted
		cap := desired - curr[i]
		floor := floorFraction * share[i] * float64(totalBatchBytes)
		if cap < floor {
			cap = floor
		}
		if cap < 0 {
			cap = 0
		}
		out[i] = cap
	}
	return out
}
