package lifecycle

import (
	"log/slog"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
)

// phaseTimings records the wall-clock cost of each phase of one batch:
// pick (controller.Pick), build (native verb construction), sign
// (signer.SignBatch), commit (testing_commitBlockV1 RPC), sensor (statecomp_get
// poll), apply (controller.Apply), journal (journal+payloads append+fsync).
//
// dispatchBatch fills pick/build/sign; commitBatch fills commit/sensor/apply/
// journal. The committer emits it once per batch via observe.
type phaseTimings struct {
	pick    time.Duration
	build   time.Duration
	sign    time.Duration
	commit  time.Duration
	sensor  time.Duration
	apply   time.Duration
	journal time.Duration
}

// observe records every phase into the Prometheus histogram (when reg is
// non-nil) and emits one INFO line summarising the batch's per-phase
// milliseconds. Called once per committed batch by the committer goroutine.
func (p phaseTimings) observe(reg *metrics.Registry, batchID uint64, txs int) {
	if reg != nil {
		h := reg.PhaseDuration
		h.WithLabelValues("pick").Observe(p.pick.Seconds())
		h.WithLabelValues("build").Observe(p.build.Seconds())
		h.WithLabelValues("sign").Observe(p.sign.Seconds())
		h.WithLabelValues("commit").Observe(p.commit.Seconds())
		h.WithLabelValues("sensor").Observe(p.sensor.Seconds())
		h.WithLabelValues("apply").Observe(p.apply.Seconds())
		h.WithLabelValues("journal").Observe(p.journal.Seconds())
	}
	slog.Info("batch timing",
		"batch_id", batchID,
		"pick_ms", ms(p.pick),
		"build_ms", ms(p.build),
		"sign_ms", ms(p.sign),
		"commit_ms", ms(p.commit),
		"sensor_ms", ms(p.sensor),
		"apply_ms", ms(p.apply),
		"journal_ms", ms(p.journal),
		"txs", txs,
	)
}

// ms converts a duration to fractional milliseconds for the timing log line.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}
