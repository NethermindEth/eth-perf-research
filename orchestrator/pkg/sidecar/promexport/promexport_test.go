package promexport

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// TestRegisterAndPublish drives a tracker snapshot through the gauges and
// asserts the exposed values. Uses an isolated registry so the test is
// hermetic and does not collide with other tests in the same binary.
func TestRegisterAndPublish(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := Register(reg)

	m.Publish(tracker.Snapshot{
		LastBlock:            1234,
		AccountsTotal:        1_000_000,
		ContractsTotal:       42_000,
		StorageSlotsTotal:    7_500_000,
		CodeBytesTotal:       1 << 30,
		ContractsWithStorage: 9_999,
		EmptyAccounts:        17,
		AccountTrieBranches:  111,
		AccountTrieLeaves:    222,
		StorageTrieBytes:     333,
	})

	if got := testutil.ToFloat64(m.accountsTotal); got != 1_000_000 {
		t.Errorf("accountsTotal = %v, want 1_000_000", got)
	}
	if got := testutil.ToFloat64(m.incrementalBlock); got != 1234 {
		t.Errorf("incrementalBlock = %v, want 1234", got)
	}
	if got := testutil.ToFloat64(m.storageSlotsTotal); got != 7_500_000 {
		t.Errorf("storageSlotsTotal = %v, want 7_500_000", got)
	}
	if got := testutil.ToFloat64(m.accountTrieLeaves); got != 222 {
		t.Errorf("accountTrieLeaves = %v, want 222", got)
	}

	// Smoke-check the histogram vec — bucket "5" set to 0 because the
	// SlotHistogram array in the snapshot is the zero value, so we just
	// confirm the vector is populated for every bucket index.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var foundHist bool
	for _, mf := range mfs {
		if mf.GetName() == "nethermind_state_comp_slot_count_histogram" {
			foundHist = true
			if got := len(mf.GetMetric()); got != 16 {
				t.Errorf("slot_count_histogram: %d series, want 16", got)
			}
		}
	}
	if !foundHist {
		t.Errorf("slot_count_histogram metric family not registered")
	}
}
