// Package metrics exposes the Prometheus collectors that replicate the 16
// orch_* metrics published by the Python orchestrator's journal exporter.
// All collectors are registered on a private prometheus.Registry so the
// process metrics (go_memstats_*, process_*) stay on this one registry and
// cardinality remains predictable.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds all metric handles and the underlying Prometheus registry.
// Callers update metrics via the exported fields directly, e.g.:
//
//	reg.JournalRecordsTotal.Inc()
//	reg.BatchID.Set(42)
type Registry struct {
	reg *prometheus.Registry

	// Counters — monotonically increasing totals.
	JournalRecordsTotal prometheus.Counter     // orch_journal_records_total
	JournalGasUsedTotal prometheus.Counter     // orch_journal_gas_used_total
	JournalVerbTotal    *prometheus.CounterVec // orch_journal_verb_total{verb}
	JournalStatusTotal  *prometheus.CounterVec // orch_journal_status_total{status}

	// Gauges — current state snapshots.
	BatchID           prometheus.Gauge
	BatchGasUsed      prometheus.Gauge
	BlockNumber       prometheus.Gauge
	DeadlineBytes     prometheus.Gauge
	InnovationRatio   prometheus.Gauge
	ResidualNorm      prometheus.Gauge
	SessionID         *prometheus.GaugeVec // orch_session_id{session_id} = 1
	ObservedFlatBytes *prometheus.GaugeVec // orch_observed_flat_bytes{axis}
	AlphaCurrent      *prometheus.GaugeVec // orch_alpha_current{verb,axis}
	AlphaState        *prometheus.GaugeVec // orch_alpha_state{verb,axis}  (sigma in Python)
	MixSimplex        *prometheus.GaugeVec // orch_mix_simplex{verb}

	// Histograms — latency distributions.
	PhaseDuration *prometheus.HistogramVec // orch_phase_duration_seconds{phase}
}

// New registers all collectors on a fresh private registry and returns the
// populated Registry. Safe to call once per process.
func New() *Registry {
	r := prometheus.NewRegistry()

	// Register standard Go runtime + process collectors on this registry so
	// they appear alongside the orch_* metrics in the same scrape.
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	journalRecordsTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "orch_journal_records_total",
		Help: "Total number of journal records appended.",
	})
	journalGasUsedTotal := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "orch_journal_gas_used_total",
		Help: "Cumulative gas used across all committed batches.",
	})
	journalVerbTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orch_journal_verb_total",
		Help: "Journal records by verb.",
	}, []string{"verb"})
	journalStatusTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orch_journal_status_total",
		Help: "Journal records by commit status.",
	}, []string{"status"})

	batchID := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_batch_id",
		Help: "Current batch ID.",
	})
	batchGasUsed := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_batch_gas_used",
		Help: "Gas used in the most recently committed batch.",
	})
	blockNumber := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_block_number",
		Help: "Block number of the most recently committed batch.",
	})
	deadlineBytes := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_deadline_bytes",
		Help: "Deadline bytes of the most recently dispatched plan.",
	})
	innovationRatio := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_innovation_ratio",
		Help: "Innovation ratio from the most recent controller update.",
	})
	residualNorm := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "orch_residual_norm",
		Help: "L2 norm of the per-axis residual from the most recent controller update.",
	})
	sessionID := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orch_session_id",
		Help: "Session identity label; value is always 1.",
	}, []string{"session_id"})
	observedFlatBytes := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orch_observed_flat_bytes",
		Help: "Observed flat trie bytes per axis (accounts, storage, code).",
	}, []string{"axis"})
	alphaCurrent := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orch_alpha_current",
		Help: "Current per-(verb,axis) learning rate α.",
	}, []string{"verb", "axis"})
	alphaState := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orch_alpha_state",
		Help: "Per-(verb,axis) innovation scale σ (kept as alpha_state for Grafana back-compat).",
	}, []string{"verb", "axis"})
	mixSimplex := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orch_mix_simplex",
		Help: "Post-projection simplex weight per verb.",
	}, []string{"verb"})
	phaseDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "orch_phase_duration_seconds",
		Help: "Per-phase batch processing latency (pick, build, sign, commit, sensor, apply, journal).",
		// Buckets span sub-millisecond bookkeeping (journal fsync) through
		// multi-second CPU work (signing tens of thousands of secp256k1 txs).
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"phase"})

	r.MustRegister(
		journalRecordsTotal,
		journalGasUsedTotal,
		journalVerbTotal,
		journalStatusTotal,
		batchID,
		batchGasUsed,
		blockNumber,
		deadlineBytes,
		innovationRatio,
		residualNorm,
		sessionID,
		observedFlatBytes,
		alphaCurrent,
		alphaState,
		mixSimplex,
		phaseDuration,
	)

	return &Registry{
		reg:                 r,
		JournalRecordsTotal: journalRecordsTotal,
		JournalGasUsedTotal: journalGasUsedTotal,
		JournalVerbTotal:    journalVerbTotal,
		JournalStatusTotal:  journalStatusTotal,
		BatchID:             batchID,
		BatchGasUsed:        batchGasUsed,
		BlockNumber:         blockNumber,
		DeadlineBytes:       deadlineBytes,
		InnovationRatio:     innovationRatio,
		ResidualNorm:        residualNorm,
		SessionID:           sessionID,
		ObservedFlatBytes:   observedFlatBytes,
		AlphaCurrent:        alphaCurrent,
		AlphaState:          alphaState,
		MixSimplex:          mixSimplex,
		PhaseDuration:       phaseDuration,
	}
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (reg *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(reg.reg, promhttp.HandlerOpts{})
}
