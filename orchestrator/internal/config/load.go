package config

import (
	"fmt"
	"math"
	"os"

	"sigs.k8s.io/yaml"
)

// mapProbe detects whether target.yaml supplies the two nested maps whose
// overlay semantics must be replace-not-merge. yaml.Unmarshal (which round-trips
// via encoding/json) merges a present map into an already-populated destination
// map rather than replacing it; for these two knobs a present block must replace
// the default map wholesale. A nil pointer means the key was absent, so the
// default is preserved.
type mapProbe struct {
	Control struct {
		BaseGasPerVerb      *map[string]uint64 `json:"base_gas_per_verb"`
		NMaxHardCeilPerVerb *map[string]int    `json:"nmax_hard_ceil_per_verb"`
	} `json:"control"`
}

// Load resolves a RunConfig for the run. Precedence, low to high:
//  1. Defaults() — the historical hardcoded constants.
//  2. The optional control: / cost: / run: blocks in target.yaml.
//
// targetYAMLPath may be empty, in which case only Defaults() applies. Keys
// present in the file overwrite the corresponding default; absent keys are
// untouched. Load validates the result and returns a clear error on any
// out-of-range value.
func Load(targetYAMLPath string) (RunConfig, error) {
	cfg := Defaults()

	if targetYAMLPath != "" {
		raw, err := os.ReadFile(targetYAMLPath)
		if err != nil {
			return RunConfig{}, fmt.Errorf("config: read %s: %w", targetYAMLPath, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return RunConfig{}, fmt.Errorf("config: parse %s: %w", targetYAMLPath, err)
		}
		// yaml.Unmarshal merges maps into the defaulted destination; for these
		// two knobs a present block must replace the default map wholesale.
		var probe mapProbe
		if err := yaml.Unmarshal(raw, &probe); err != nil {
			return RunConfig{}, fmt.Errorf("config: parse %s: %w", targetYAMLPath, err)
		}
		if probe.Control.BaseGasPerVerb != nil {
			cfg.Control.BaseGasPerVerb = *probe.Control.BaseGasPerVerb
		}
		if probe.Control.NMaxHardCeilPerVerb != nil {
			cfg.Control.NMaxHardCeilPerVerb = *probe.Control.NMaxHardCeilPerVerb
		}
	}

	if err := cfg.validate(); err != nil {
		return RunConfig{}, err
	}
	return cfg, nil
}

// Validate range-checks a RunConfig. Exported so callers that mutate a loaded
// config (e.g. CLI-flag overrides) can re-check before use.
func Validate(c RunConfig) error { return c.validate() }

func (c RunConfig) validate() error {
	ck := c.Control
	if !inUnit(ck.Epsilon) {
		return rangeErr("control.epsilon", ck.Epsilon, "[0, 1]")
	}
	if ck.ToleranceFloor < 0 {
		return rangeErr("control.tolerance_floor", ck.ToleranceFloor, ">= 0")
	}
	if !inUnit(ck.GasFillFraction) || ck.GasFillFraction <= 0 {
		return rangeErr("control.gas_fill_fraction", ck.GasFillFraction, "(0, 1]")
	}
	if !inUnit(ck.GasCapFraction) || ck.GasCapFraction <= 0 {
		return rangeErr("control.gas_cap_fraction", ck.GasCapFraction, "(0, 1]")
	}
	if ck.GasFillFraction > ck.GasCapFraction {
		return fmt.Errorf("config: control.gas_fill_fraction (%g) must not exceed control.gas_cap_fraction (%g)",
			ck.GasFillFraction, ck.GasCapFraction)
	}
	if ck.NMaxHardCeil < 1 {
		return rangeErr("control.nmax_hard_ceil", float64(ck.NMaxHardCeil), ">= 1")
	}
	if ck.AlphaMin < 0 || ck.AlphaMax < ck.AlphaMin {
		return fmt.Errorf("config: control.alpha_min (%g) / alpha_max (%g) invalid: need 0 <= alpha_min <= alpha_max",
			ck.AlphaMin, ck.AlphaMax)
	}
	if ck.SigmoidK <= 0 {
		return rangeErr("control.sigmoid_k", ck.SigmoidK, "> 0")
	}
	if ck.SigmaFloor <= 0 {
		return rangeErr("control.sigma_floor", ck.SigmaFloor, "> 0")
	}
	if ck.AlphaEpsilon <= 0 {
		return rangeErr("control.alpha_epsilon", ck.AlphaEpsilon, "> 0")
	}
	if !inUnit(ck.SigmaEWMADecay) {
		return rangeErr("control.sigma_ewma_decay", ck.SigmaEWMADecay, "[0, 1]")
	}
	if !inUnit(ck.AlphaEWMADecay) {
		return rangeErr("control.alpha_ewma_decay", ck.AlphaEWMADecay, "[0, 1]")
	}
	if ck.CoeffBound <= 0 {
		return rangeErr("control.coeff_bound", ck.CoeffBound, "> 0")
	}
	if !inUnit(ck.EWMAAlpha) || ck.EWMAAlpha <= 0 {
		return rangeErr("control.ewma_alpha", ck.EWMAAlpha, "(0, 1]")
	}
	if ck.VerbStatsColdStartN < 1 {
		return rangeErr("control.verbstats_cold_start_n", float64(ck.VerbStatsColdStartN), ">= 1")
	}
	if ck.OvershootThreshold <= 0 {
		return rangeErr("control.overshoot_threshold", ck.OvershootThreshold, "> 0")
	}
	if ck.OvershootWindow < 1 {
		return rangeErr("control.overshoot_window", float64(ck.OvershootWindow), ">= 1")
	}
	if ck.OvershootMaxTrips < 1 || ck.OvershootMaxTrips > ck.OvershootWindow {
		return fmt.Errorf("config: control.overshoot_max_trips (%d) must be in [1, overshoot_window=%d]",
			ck.OvershootMaxTrips, ck.OvershootWindow)
	}
	if ck.OvershootGrace < 0 {
		return rangeErr("control.overshoot_grace", float64(ck.OvershootGrace), ">= 0")
	}
	if ck.ResidualNormFloor <= 0 {
		return rangeErr("control.residual_norm_floor", ck.ResidualNormFloor, "> 0")
	}
	if ck.DefaultAvgTxRLP <= 0 {
		return rangeErr("control.default_avg_tx_rlp", ck.DefaultAvgTxRLP, "> 0")
	}
	if ck.DefaultSigma <= 0 {
		return rangeErr("control.default_sigma", ck.DefaultSigma, "> 0")
	}
	if !inUnit(ck.AvgTxRLPDecayNew) {
		return rangeErr("control.avg_tx_rlp_decay_new", ck.AvgTxRLPDecayNew, "[0, 1]")
	}
	if ck.DefaultBaseGasPerVerb == 0 {
		return rangeErr("control.default_base_gas_per_verb", 0, "> 0")
	}
	for verb, gas := range ck.BaseGasPerVerb {
		if gas == 0 {
			return fmt.Errorf("config: control.base_gas_per_verb[%q] is 0, must be > 0", verb)
		}
	}
	for verb, ceil := range ck.NMaxHardCeilPerVerb {
		if ceil < 1 {
			return rangeErr(fmt.Sprintf("control.nmax_hard_ceil_per_verb[%q]", verb), float64(ceil), ">= 1")
		}
	}

	cost := c.Cost
	if cost.PriorityTipWei < 0 {
		return rangeErr("cost.priority_tip_wei", float64(cost.PriorityTipWei), ">= 0")
	}
	if cost.FeePolicyIntervalMS <= 0 {
		return rangeErr("cost.fee_policy_interval_ms", float64(cost.FeePolicyIntervalMS), "> 0")
	}
	if cost.EthPerGasTarget < 0 {
		return rangeErr("cost.eth_per_gas_target", float64(cost.EthPerGasTarget), ">= 0")
	}

	r := c.Run
	if r.TotalBatchBytesCap < 1024 {
		return rangeErr("run.total_batch_bytes_cap", float64(r.TotalBatchBytesCap), ">= 1024")
	}
	if r.AddressStride == 0 {
		return rangeErr("run.address_stride", 0, "> 0")
	}
	if r.DispatchSkipStreakHalt < 1 {
		return rangeErr("run.dispatch_skip_streak_halt", float64(r.DispatchSkipStreakHalt), ">= 1")
	}
	if r.RejectionStreakHalt < 1 {
		return rangeErr("run.rejection_streak_halt", float64(r.RejectionStreakHalt), ">= 1")
	}
	if r.IdleBackoffMS < 0 {
		return rangeErr("run.idle_backoff_ms", float64(r.IdleBackoffMS), ">= 0")
	}
	if r.ResumeReorgTolerance < 0 {
		return rangeErr("run.resume_reorg_tolerance", float64(r.ResumeReorgTolerance), ">= 0")
	}
	if r.RPCTimeoutS <= 0 {
		return rangeErr("run.rpc_timeout_s", float64(r.RPCTimeoutS), "> 0")
	}
	if r.SensorPollIntervalMS <= 0 {
		return rangeErr("run.sensor_poll_interval_ms", float64(r.SensorPollIntervalMS), "> 0")
	}
	if r.SensorDeadlineMS <= 0 {
		return rangeErr("run.sensor_deadline_ms", float64(r.SensorDeadlineMS), "> 0")
	}
	if r.SensorPollGapMS <= 0 {
		return rangeErr("run.sensor_poll_gap_ms", float64(r.SensorPollGapMS), "> 0")
	}
	if r.SensorLogIntervalS <= 0 {
		return rangeErr("run.sensor_log_interval_s", float64(r.SensorLogIntervalS), "> 0")
	}
	if r.MetricsAddr == "" {
		return fmt.Errorf("config: run.metrics_addr must not be empty")
	}
	if r.TargetWatchDebounceMS <= 0 {
		return rangeErr("run.target_watch_debounce_ms", float64(r.TargetWatchDebounceMS), "> 0")
	}
	if r.ContractDeployGas == 0 {
		return rangeErr("run.contract_deploy_gas", 0, "> 0")
	}
	if r.ReconnectMaxWaitS <= 0 {
		return rangeErr("run.reconnect_max_wait_s", float64(r.ReconnectMaxWaitS), "> 0")
	}
	if r.ReconnectBackoffInitialMS <= 0 {
		return rangeErr("run.reconnect_backoff_initial_ms", float64(r.ReconnectBackoffInitialMS), "> 0")
	}
	if r.ReconnectBackoffMaxMS < r.ReconnectBackoffInitialMS {
		return fmt.Errorf("config: run.reconnect_backoff_max_ms (%d) must not be below run.reconnect_backoff_initial_ms (%d)",
			r.ReconnectBackoffMaxMS, r.ReconnectBackoffInitialMS)
	}
	return nil
}

func inUnit(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

func rangeErr(key string, v float64, want string) error {
	return fmt.Errorf("config: %s=%g out of range, want %s", key, v, want)
}
