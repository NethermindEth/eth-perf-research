// Package promexport publishes the sidecar's tier-1 state-composition counters
// as Prometheus gauges under the legacy Nethermind.StateComposition metric
// names (`nethermind_state_comp_*`). This preserves drop-in compatibility with
// the bloatnet Grafana dashboard that was originally written against the
// in-process NM plugin (now disabled in v19).
//
// Only counters that the Go tracker actually owns are exported. Panels that
// depend on metrics the sidecar doesn't compute yet (depth-bucketed
// trie_depth_{nodes,bytes}, scan timing, diffs_applied/diff_errors counters,
// avg/max depth & branch occupancy) intentionally stay absent — they will need
// follow-up tracker work before they can be wired up.
package promexport

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/NethermindEth/eth-perf-research/orchestrator/pkg/sidecar/tracker"
)

// Metrics is the set of gauges we publish. All names match the
// Nethermind.StateComposition plugin's legacy Prometheus output so the
// existing bloatnet dashboard panels keep working unchanged.
type Metrics struct {
	accountsTotal         prometheus.Gauge
	contractsTotal        prometheus.Gauge
	storageSlotsTotal     prometheus.Gauge
	codeBytesTotal        prometheus.Gauge
	contractsWithStorage  prometheus.Gauge
	emptyAccounts         prometheus.Gauge
	accountTrieBranches   prometheus.Gauge
	accountTrieExtensions prometheus.Gauge
	accountTrieLeaves     prometheus.Gauge
	accountTrieBytes      prometheus.Gauge
	storageTrieBranches   prometheus.Gauge
	storageTrieExtensions prometheus.Gauge
	storageTrieLeaves     prometheus.Gauge
	storageTrieBytes      prometheus.Gauge
	incrementalBlock      prometheus.Gauge
	slotCountHistogram    *prometheus.GaugeVec

	// Derived totals + shares (acct/stor/code as fraction of total).
	totalStateBytes prometheus.Gauge
	accountShare    prometheus.Gauge
	storageShare    prometheus.Gauge
	codeShare       prometheus.Gauge

	// Growth-rate gauges in bytes/sec, EWMA over a ~30s window. Useful for
	// Grafana single-stat panels that show "current speed" without needing
	// rate() math at query time.
	totalGrowthBPS   prometheus.Gauge
	accountGrowthBPS prometheus.Gauge
	storageGrowthBPS prometheus.Gauge
	codeGrowthBPS    prometheus.Gauge

	// Rate-tracking state (private to Publish; not exported).
	lastSample time.Time
	lastAcct   int64
	lastStor   int64
	lastCode   int64
}

// Register builds the metric set on the given registerer. Pass nil to use
// the default registry — that's the production path. Tests pass a fresh
// registry so they can run alongside other tests without colliding on the
// global default. Call once per registerer.
func Register(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := promauto.With(reg)
	return &Metrics{
		accountsTotal: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_accounts_total",
			Help: "Total number of accounts in the state trie.",
		}),
		contractsTotal: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_contracts_total",
			Help: "Total number of contracts (accounts with non-empty code).",
		}),
		storageSlotsTotal: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_slots_total",
			Help: "Approximated total storage slot count across all contracts.",
		}),
		codeBytesTotal: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_code_bytes_total",
			Help: "Aggregate contract bytecode size across all unique code hashes (refcount-weighted).",
		}),
		contractsWithStorage: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_contracts_with_storage",
			Help: "Number of contracts that have at least one storage slot.",
		}),
		emptyAccounts: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_empty_accounts",
			Help: "Number of accounts with zero balance, nonce and no code.",
		}),
		accountTrieBranches: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_trie_branches",
			Help: "Branch nodes in the account trie.",
		}),
		accountTrieExtensions: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_trie_extensions",
			Help: "Extension nodes in the account trie.",
		}),
		accountTrieLeaves: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_trie_leaves",
			Help: "Leaf nodes in the account trie.",
		}),
		accountTrieBytes: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_trie_bytes",
			Help: "Total encoded byte size of all account-trie nodes.",
		}),
		storageTrieBranches: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_trie_branches",
			Help: "Branch nodes across all storage tries.",
		}),
		storageTrieExtensions: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_trie_extensions",
			Help: "Extension nodes across all storage tries.",
		}),
		storageTrieLeaves: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_trie_leaves",
			Help: "Leaf nodes across all storage tries.",
		}),
		storageTrieBytes: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_trie_bytes",
			Help: "Total encoded byte size of all storage-trie nodes.",
		}),
		incrementalBlock: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_incremental_block",
			Help: "Highest block number folded into the tracker by the diff tailer.",
		}),
		slotCountHistogram: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_slot_count_histogram",
			Help: "Per-bucket contract counts in the slot-count histogram.",
		}, []string{"bucket"}),
		totalStateBytes: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_total_bytes",
			Help: "Sum of accountTrieBytes + storageTrieBytes + codeBytesTotal.",
		}),
		accountShare: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_share",
			Help: "accountTrieBytes / totalStateBytes (0..1).",
		}),
		storageShare: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_share",
			Help: "storageTrieBytes / totalStateBytes (0..1).",
		}),
		codeShare: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_code_share",
			Help: "codeBytesTotal / totalStateBytes (0..1).",
		}),
		totalGrowthBPS: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_growth_bytes_per_second",
			Help: "Total state growth rate in bytes/sec (EWMA over ~30s).",
		}),
		accountGrowthBPS: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_account_growth_bytes_per_second",
			Help: "Account-trie growth rate in bytes/sec (EWMA over ~30s).",
		}),
		storageGrowthBPS: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_storage_growth_bytes_per_second",
			Help: "Storage-trie growth rate in bytes/sec (EWMA over ~30s).",
		}),
		codeGrowthBPS: factory.NewGauge(prometheus.GaugeOpts{
			Name: "nethermind_state_comp_code_growth_bytes_per_second",
			Help: "Code growth rate in bytes/sec (EWMA over ~30s).",
		}),
	}
}

// Publish refreshes all gauges from a tracker snapshot. Cheap (O(shards) for
// the snapshot, then constant time per metric) — safe to call every second.
func (m *Metrics) Publish(snap tracker.Snapshot) {
	m.accountsTotal.Set(float64(snap.AccountsTotal))
	m.contractsTotal.Set(float64(snap.ContractsTotal))
	m.storageSlotsTotal.Set(float64(snap.StorageSlotsTotal))
	m.codeBytesTotal.Set(float64(snap.CodeBytesTotal))
	m.contractsWithStorage.Set(float64(snap.ContractsWithStorage))
	m.emptyAccounts.Set(float64(snap.EmptyAccounts))
	m.accountTrieBranches.Set(float64(snap.AccountTrieBranches))
	m.accountTrieExtensions.Set(float64(snap.AccountTrieExtensions))
	m.accountTrieLeaves.Set(float64(snap.AccountTrieLeaves))
	m.accountTrieBytes.Set(float64(snap.AccountTrieBytes))
	m.storageTrieBranches.Set(float64(snap.StorageTrieBranches))
	m.storageTrieExtensions.Set(float64(snap.StorageTrieExtensions))
	m.storageTrieLeaves.Set(float64(snap.StorageTrieLeaves))
	m.storageTrieBytes.Set(float64(snap.StorageTrieBytes))
	m.incrementalBlock.Set(float64(snap.LastBlock))
	for i, v := range snap.SlotHistogram {
		m.slotCountHistogram.WithLabelValues(strconv.Itoa(i)).Set(float64(v))
	}

	total := snap.AccountTrieBytes + snap.StorageTrieBytes + snap.CodeBytesTotal
	m.totalStateBytes.Set(float64(total))
	if total > 0 {
		m.accountShare.Set(float64(snap.AccountTrieBytes) / float64(total))
		m.storageShare.Set(float64(snap.StorageTrieBytes) / float64(total))
		m.codeShare.Set(float64(snap.CodeBytesTotal) / float64(total))
	}

	// EWMA growth rate. α=0.1 → ~30s effective window at 1Hz publish.
	now := time.Now()
	if !m.lastSample.IsZero() {
		dt := now.Sub(m.lastSample).Seconds()
		if dt > 0 {
			const a = 0.1
			dAcct := float64(snap.AccountTrieBytes-m.lastAcct) / dt
			dStor := float64(snap.StorageTrieBytes-m.lastStor) / dt
			dCode := float64(snap.CodeBytesTotal-m.lastCode) / dt
			m.accountGrowthBPS.Set(a*dAcct + (1-a)*currentGauge(m.accountGrowthBPS))
			m.storageGrowthBPS.Set(a*dStor + (1-a)*currentGauge(m.storageGrowthBPS))
			m.codeGrowthBPS.Set(a*dCode + (1-a)*currentGauge(m.codeGrowthBPS))
			m.totalGrowthBPS.Set(a*(dAcct+dStor+dCode) + (1-a)*currentGauge(m.totalGrowthBPS))
		}
	}
	m.lastSample = now
	m.lastAcct = snap.AccountTrieBytes
	m.lastStor = snap.StorageTrieBytes
	m.lastCode = snap.CodeBytesTotal
}

// currentGauge reads back a gauge's current value for EWMA chaining.
func currentGauge(g prometheus.Gauge) float64 {
	var m dto.Metric
	if err := g.Write(&m); err != nil || m.Gauge == nil || m.Gauge.Value == nil {
		return 0
	}
	return *m.Gauge.Value
}

// Run drives a refresh loop until ctx is cancelled. Pulls a snapshot from the
// tracker every interval and republishes the gauges. Returns when ctx is done.
func (m *Metrics) Run(ctx context.Context, t *tracker.Tracker, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	// One immediate publish so /metrics is non-empty as soon as the tracker
	// exists, instead of waiting interval seconds for the first tick.
	m.Publish(t.SnapshotCounters())
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.Publish(t.SnapshotCounters())
		}
	}
}
