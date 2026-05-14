package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
)

func TestNewRegistry(t *testing.T) {
	reg := metrics.New()
	if reg == nil {
		t.Fatal("New() returned nil")
	}
}

func TestCountersIncrement(t *testing.T) {
	reg := metrics.New()

	reg.JournalRecordsTotal.Inc()
	reg.JournalRecordsTotal.Inc()
	reg.JournalGasUsedTotal.Add(1_000_000)

	reg.JournalVerbTotal.WithLabelValues("eoatx").Inc()
	reg.JournalVerbTotal.WithLabelValues("calltx").Inc()
	reg.JournalVerbTotal.WithLabelValues("eoatx").Inc()

	reg.JournalStatusTotal.WithLabelValues("ok").Inc()

	body := scrape(t, reg)

	assertContains(t, body, "orch_journal_records_total 2")
	assertContains(t, body, "orch_journal_gas_used_total 1e+06")
	assertContains(t, body, `orch_journal_verb_total{verb="eoatx"} 2`)
	assertContains(t, body, `orch_journal_verb_total{verb="calltx"} 1`)
	assertContains(t, body, `orch_journal_status_total{status="ok"} 1`)
}

func TestGaugesSet(t *testing.T) {
	reg := metrics.New()

	reg.BatchID.Set(42)
	reg.BatchGasUsed.Set(3_000_000)
	reg.BlockNumber.Set(100)
	reg.DeadlineBytes.Set(8388608)
	reg.Epsilon.Set(0.05)
	reg.InnovationRatio.Set(0.12)
	reg.ResidualNorm.Set(0.003)

	body := scrape(t, reg)

	assertContains(t, body, "orch_batch_id 42")
	assertContains(t, body, "orch_batch_gas_used 3e+06")
	assertContains(t, body, "orch_block_number 100")
	assertContains(t, body, "orch_epsilon 0.05")
	assertContains(t, body, "orch_residual_norm 0.003")
}

func TestGaugeVecs(t *testing.T) {
	reg := metrics.New()

	reg.SessionID.WithLabelValues("abc123").Set(1)
	reg.ObservedFlatBytes.WithLabelValues("accounts").Set(1_000_000)
	reg.ObservedFlatBytes.WithLabelValues("storage").Set(2_000_000)
	reg.ObservedFlatBytes.WithLabelValues("code").Set(500_000)
	reg.AlphaCurrent.WithLabelValues("eoatx", "accounts").Set(0.02)
	reg.AlphaState.WithLabelValues("eoatx", "storage").Set(1.0)
	reg.MixSimplex.WithLabelValues("eoatx").Set(0.5)
	reg.MixSimplex.WithLabelValues("calltx").Set(0.5)

	body := scrape(t, reg)

	assertContains(t, body, `orch_session_id{session_id="abc123"} 1`)
	assertContains(t, body, `orch_observed_flat_bytes{axis="accounts"} 1e+06`)
	assertContains(t, body, `orch_observed_flat_bytes{axis="storage"} 2e+06`)
	assertContains(t, body, `orch_observed_flat_bytes{axis="code"} 500000`)
	assertContains(t, body, `orch_alpha_current{axis="accounts",verb="eoatx"} 0.02`)
	assertContains(t, body, `orch_alpha_state{axis="storage",verb="eoatx"} 1`)
	assertContains(t, body, `orch_mix_simplex{verb="eoatx"} 0.5`)
	assertContains(t, body, `orch_mix_simplex{verb="calltx"} 0.5`)
}

func TestLabelVecChildrenIsolate(t *testing.T) {
	reg := metrics.New()

	reg.JournalVerbTotal.WithLabelValues("eoatx").Inc()
	reg.JournalVerbTotal.WithLabelValues("eoatx").Inc()
	reg.JournalVerbTotal.WithLabelValues("calltx").Inc()

	body := scrape(t, reg)

	assertContains(t, body, `orch_journal_verb_total{verb="eoatx"} 2`)
	assertContains(t, body, `orch_journal_verb_total{verb="calltx"} 1`)

	if strings.Contains(body, `orch_journal_verb_total{verb="calltx"} 2`) {
		t.Error("calltx count incorrectly shows 2, isolation failed")
	}
}

func TestMetricNamesPresent(t *testing.T) {
	reg := metrics.New()
	body := scrape(t, reg)

	names := []string{
		"orch_journal_records_total",
		"orch_journal_gas_used_total",
		"orch_batch_id",
		"orch_batch_gas_used",
		"orch_block_number",
		"orch_deadline_bytes",
		"orch_epsilon",
		"orch_innovation_ratio",
		"orch_residual_norm",
	}
	for _, name := range names {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q not found in /metrics output", name)
		}
	}
}

// scrape hits the registry's Handler and returns the response body.
func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	reg.Handler().ServeHTTP(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics handler returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	return string(body)
}

func assertContains(t *testing.T, body, substr string) {
	t.Helper()
	if !strings.Contains(body, substr) {
		t.Errorf("metrics body does not contain %q\nbody excerpt:\n%s", substr,
			firstLines(body, 30))
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
