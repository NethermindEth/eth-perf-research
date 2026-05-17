package controller

import (
	"math"
	"testing"
)

// sRatioFlowCap calls RatioFlowCap (now a State method) on a State built with
// the default RunConfig, preserving the pre-RunConfig free-function ergonomics.
func sRatioFlowCap(obs *Observation, t *Target, totalBatchBytes int) [3]float64 {
	s := &State{cfg: testCfg()}
	return s.RatioFlowCap(obs, t, totalBatchBytes)
}

func TestRatioFlowCapOverServedReturnsFloor(t *testing.T) {
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.273, AxisStorage: 0.667, AxisCode: 0.060},
		TotalBytes: 1_000_000_000_000,
	}
	obs := &Observation{
		AccountTrieBytes: 100_000_000_000,
		StorageTrieBytes: 700_000_000_000, // over-served (target 66.7%, actual 87.5%)
		CodeBytesTotal:   50_000_000_000,
	}
	totalBatchBytes := 8 * 1024 * 1024

	caps := sRatioFlowCap(obs, tgt, totalBatchBytes)
	expectedFloor := testFloorFraction * 0.667 * float64(totalBatchBytes)
	if math.Abs(caps[1]-expectedFloor) > 1e-3 {
		t.Errorf("storage cap=%.0f, want floor=%.0f", caps[1], expectedFloor)
	}
}

func TestRatioFlowCapUnderServedReturnsLargeHeadroom(t *testing.T) {
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.273, AxisStorage: 0.667, AxisCode: 0.060},
		TotalBytes: 1_000_000_000_000,
	}
	obs := &Observation{
		AccountTrieBytes: 10_000_000_000, // under-served
		StorageTrieBytes: 700_000_000_000,
		CodeBytesTotal:   50_000_000_000,
	}
	totalBatchBytes := 8 * 1024 * 1024

	caps := sRatioFlowCap(obs, tgt, totalBatchBytes)
	expectedFloor := testFloorFraction * 0.273 * float64(totalBatchBytes)
	if caps[0] <= expectedFloor {
		t.Errorf("under-served accounts cap=%.0f, expected > floor=%.0f", caps[0], expectedFloor)
	}
}

func TestRatioFlowCapZeroObsReturnsFloors(t *testing.T) {
	tgt := &Target{
		Shares:     map[Axis]float64{AxisAccounts: 0.273, AxisStorage: 0.667, AxisCode: 0.060},
		TotalBytes: 1_000_000_000_000,
	}
	totalBatchBytes := 8 * 1024 * 1024
	caps := sRatioFlowCap(&Observation{}, tgt, totalBatchBytes)
	for i, share := range []float64{0.273, 0.667, 0.060} {
		// With zero obs, share*newTotal*(1+tol) lands far above the floor.
		floor := testFloorFraction * share * float64(totalBatchBytes)
		if caps[i] < floor {
			t.Errorf("axis %d cap=%.0f < floor=%.0f", i, caps[i], floor)
		}
	}
}

func TestRatioFlowCapNilInputs(t *testing.T) {
	caps := sRatioFlowCap(nil, nil, 0)
	for i, c := range caps {
		if !math.IsInf(c, 1) {
			t.Errorf("axis %d: nil inputs should return +Inf, got %v", i, c)
		}
	}
}
