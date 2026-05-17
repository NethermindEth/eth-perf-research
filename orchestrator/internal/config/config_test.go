package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultsMatchHistoricalConstants pins every Defaults() field to the value
// the orchestrator hardcoded before RunConfig. A drift here is a behaviour
// change — the refactor is meant to be behaviour-neutral.
func TestDefaultsMatchHistoricalConstants(t *testing.T) {
	d := Defaults()

	ck := d.Control
	checkF(t, "epsilon", ck.Epsilon, 0.5)
	checkF(t, "projection_eta", ck.ProjectionEta, 0.1)
	checkF(t, "tolerance_floor", ck.ToleranceFloor, 0.005)
	checkF(t, "entropy_floor", ck.EntropyFloor, 0.02)
	if !ck.AntiWindupEnabled {
		t.Errorf("anti_windup_enabled = false, want true")
	}
	if !ck.BytesPerGasTieBreak {
		t.Errorf("bytes_per_gas_tie_break = false, want true")
	}
	checkF(t, "gas_fill_fraction", ck.GasFillFraction, 0.90)
	checkF(t, "gas_cap_fraction", ck.GasCapFraction, 0.95)
	checkI(t, "nmax_hard_ceil", ck.NMaxHardCeil, 12000)
	checkF(t, "alpha_min", ck.AlphaMin, 0.02)
	checkF(t, "alpha_max", ck.AlphaMax, 0.30)
	checkF(t, "sigmoid_center", ck.SigmoidCenter, 0.08)
	checkF(t, "sigmoid_k", ck.SigmoidK, 25.0)
	checkF(t, "sigma_floor", ck.SigmaFloor, 1.0)
	checkF(t, "alpha_epsilon", ck.AlphaEpsilon, 1000.0)
	checkF(t, "sigma_ewma_decay", ck.SigmaEWMADecay, 0.9)
	checkF(t, "alpha_ewma_decay", ck.AlphaEWMADecay, 0.7)
	checkF(t, "coeff_bound", ck.CoeffBound, 1e6)
	checkF(t, "ewma_alpha", ck.EWMAAlpha, 0.10)
	if ck.VerbStatsColdStartN != 10 {
		t.Errorf("verbstats_cold_start_n = %d, want 10", ck.VerbStatsColdStartN)
	}
	checkF(t, "overshoot_threshold", ck.OvershootThreshold, 0.20)
	checkI(t, "overshoot_window", ck.OvershootWindow, 5)
	checkI(t, "overshoot_max_trips", ck.OvershootMaxTrips, 3)
	checkI(t, "overshoot_grace", ck.OvershootGrace, 5)
	checkF(t, "residual_norm_floor", ck.ResidualNormFloor, 1024.0)
	checkF(t, "default_avg_tx_rlp", ck.DefaultAvgTxRLP, 1500.0)
	checkF(t, "default_sigma", ck.DefaultSigma, 1.0)
	checkF(t, "avg_tx_rlp_decay_new", ck.AvgTxRLPDecayNew, 0.3)
	if ck.DefaultBaseGasPerVerb != 1_000_000 {
		t.Errorf("default_base_gas_per_verb = %d, want 1000000", ck.DefaultBaseGasPerVerb)
	}
	wantGas := map[string]uint64{
		"eoatx": 21_000, "deploytx": 1_000_000, "factorydeploytx": 500_000,
		"storagespam": 2_050_000, "storagerefundtx": 3_000_000, "erc20tx": 100_000,
		"erc20_bloater": 16_700_000, "uniswap_swaps": 200_000, "gasburnertx": 2_000_000,
		"calltx": 500_000,
	}
	for verb, want := range wantGas {
		if got := ck.BaseGasPerVerb[verb]; got != want {
			t.Errorf("base_gas_per_verb[%q] = %d, want %d", verb, got, want)
		}
	}

	cost := d.Cost
	if cost.PriorityTipWei != 1_000_000_000 {
		t.Errorf("priority_tip_wei = %d, want 1000000000", cost.PriorityTipWei)
	}
	if cost.FeePolicyIntervalMS != 1000 {
		t.Errorf("fee_policy_interval_ms = %d, want 1000", cost.FeePolicyIntervalMS)
	}
	if cost.EthPerGasTarget != 1_000_000_000 {
		t.Errorf("eth_per_gas_target = %d, want 1000000000", cost.EthPerGasTarget)
	}

	r := d.Run
	if r.TotalBatchBytes != 2*1024*1024 {
		t.Errorf("total_batch_bytes = %d, want %d", r.TotalBatchBytes, 2*1024*1024)
	}
	if r.TotalBatchBytesCap != 7680*1024 {
		t.Errorf("total_batch_bytes_cap = %d, want %d", r.TotalBatchBytesCap, 7680*1024)
	}
	if r.AddressStride != uint64(1)<<40 {
		t.Errorf("address_stride = %d, want %d", r.AddressStride, uint64(1)<<40)
	}
	checkI(t, "dispatch_skip_streak_halt", r.DispatchSkipStreakHalt, 20)
	checkI(t, "rejection_streak_halt", r.RejectionStreakHalt, 5)
	if r.IdleBackoffMS != 50 {
		t.Errorf("idle_backoff_ms = %d, want 50", r.IdleBackoffMS)
	}
	if r.ResumeReorgTolerance != 256 {
		t.Errorf("resume_reorg_tolerance = %d, want 256", r.ResumeReorgTolerance)
	}
	if r.RPCTimeoutS != 120 {
		t.Errorf("rpc_timeout_s = %d, want 120", r.RPCTimeoutS)
	}
	if r.SensorPollIntervalMS != 100 || r.SensorDeadlineMS != 2000 ||
		r.SensorPollGapMS != 2000 || r.SensorLogIntervalS != 30 {
		t.Errorf("sensor intervals drifted: %+v", r)
	}
	if r.MetricsAddr != ":9101" {
		t.Errorf("metrics_addr = %q, want :9101", r.MetricsAddr)
	}
	if r.TargetWatchDebounceMS != 100 {
		t.Errorf("target_watch_debounce_ms = %d, want 100", r.TargetWatchDebounceMS)
	}
	if r.ContractDeployGas != 2_000_000 {
		t.Errorf("contract_deploy_gas = %d, want 2000000", r.ContractDeployGas)
	}
	if r.ReconnectMaxWaitS != 1800 {
		t.Errorf("reconnect_max_wait_s = %d, want 1800", r.ReconnectMaxWaitS)
	}
	if r.ReconnectBackoffInitialMS != 500 {
		t.Errorf("reconnect_backoff_initial_ms = %d, want 500", r.ReconnectBackoffInitialMS)
	}
	if r.ReconnectBackoffMaxMS != 15000 {
		t.Errorf("reconnect_backoff_max_ms = %d, want 15000", r.ReconnectBackoffMaxMS)
	}
}

// TestLoadOverlaysYAMLBlock verifies a control:/cost:/run: block in target.yaml
// overlays the matching Defaults() fields while every absent key is preserved.
func TestLoadOverlaysYAMLBlock(t *testing.T) {
	yaml := `
shares:
  accounts: 0.5
  storage: 0.5
total_bytes: 1000000
control:
  epsilon: 0.25
  nmax_hard_ceil: 9000
  base_gas_per_verb:
    eoatx: 30000
cost:
  priority_tip_wei: 5000000000
run:
  metrics_addr: ":9999"
  rpc_timeout_s: 300
`
	path := writeYAML(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Control.Epsilon != 0.25 {
		t.Errorf("epsilon = %g, want 0.25 (YAML overlay)", cfg.Control.Epsilon)
	}
	if cfg.Control.NMaxHardCeil != 9000 {
		t.Errorf("nmax_hard_ceil = %d, want 9000 (YAML overlay)", cfg.Control.NMaxHardCeil)
	}
	if cfg.Control.BaseGasPerVerb["eoatx"] != 30000 {
		t.Errorf("base_gas_per_verb[eoatx] = %d, want 30000 (YAML overlay)",
			cfg.Control.BaseGasPerVerb["eoatx"])
	}
	if cfg.Cost.PriorityTipWei != 5_000_000_000 {
		t.Errorf("priority_tip_wei = %d, want 5000000000 (YAML overlay)", cfg.Cost.PriorityTipWei)
	}
	if cfg.Run.MetricsAddr != ":9999" || cfg.Run.RPCTimeoutS != 300 {
		t.Errorf("run overlay not applied: %+v", cfg.Run)
	}
	// An absent key keeps its default.
	if cfg.Control.ProjectionEta != Defaults().Control.ProjectionEta {
		t.Errorf("projection_eta = %g, want default (absent key must not change)",
			cfg.Control.ProjectionEta)
	}
}

// TestLoadEnvOverridesYAML verifies an env var takes precedence over a YAML
// value, which in turn takes precedence over Defaults().
func TestLoadEnvOverridesYAML(t *testing.T) {
	yaml := `
control:
  epsilon: 0.25
`
	path := writeYAML(t, yaml)
	t.Setenv("ORCH_EPSILON", "0.77")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Control.Epsilon != 0.77 {
		t.Errorf("epsilon = %g, want 0.77 (env must override YAML)", cfg.Control.Epsilon)
	}
}

// TestLoadFailsFastOnBadValue verifies an out-of-range value (here from a YAML
// block) is rejected at Load time with a clear error.
func TestLoadFailsFastOnBadValue(t *testing.T) {
	yaml := `
control:
  gas_fill_fraction: 1.5
`
	path := writeYAML(t, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to fail on gas_fill_fraction=1.5, got nil")
	}
}

// TestLoadFailsFastOnBadEnv verifies a malformed env override is a hard error.
func TestLoadFailsFastOnBadEnv(t *testing.T) {
	t.Setenv("ORCH_RPC_TIMEOUT_S", "not-a-number")
	if _, err := Load(""); err == nil {
		t.Fatal("expected Load to fail on malformed ORCH_RPC_TIMEOUT_S, got nil")
	}
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write target.yaml: %v", err)
	}
	return path
}

func checkF(t *testing.T, name string, got, want float64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %g, want %g", name, got, want)
	}
}

func checkI(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", name, got, want)
	}
}
