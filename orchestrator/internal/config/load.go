package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// yamlFile is the subset of target.yaml the config loader reads. The shares /
// total_bytes blocks belong to the target package and are intentionally left
// out here: they unmarshal independently and are unaffected by config parsing.
// control / cost / run are all optional — a missing block, or a missing key
// within a block, falls through to Defaults().
type yamlFile struct {
	Control *controlYAML `json:"control"`
	Cost    *costYAML    `json:"cost"`
	Run     *runYAML     `json:"run"`
}

// The *YAML structs use pointer fields so a key absent from target.yaml stays
// nil and the corresponding Defaults() value is preserved; only keys actually
// present in the file overlay the default.
type controlYAML struct {
	Epsilon               *float64          `json:"epsilon"`
	ProjectionEta         *float64          `json:"projection_eta"`
	ToleranceFloor        *float64          `json:"tolerance_floor"`
	GasFillFraction       *float64          `json:"gas_fill_fraction"`
	GasCapFraction        *float64          `json:"gas_cap_fraction"`
	NMaxHardCeil          *int              `json:"nmax_hard_ceil"`
	AlphaMin              *float64          `json:"alpha_min"`
	AlphaMax              *float64          `json:"alpha_max"`
	SigmoidCenter         *float64          `json:"sigmoid_center"`
	SigmoidK              *float64          `json:"sigmoid_k"`
	SigmaFloor            *float64          `json:"sigma_floor"`
	AlphaEpsilon          *float64          `json:"alpha_epsilon"`
	SigmaEWMADecay        *float64          `json:"sigma_ewma_decay"`
	AlphaEWMADecay        *float64          `json:"alpha_ewma_decay"`
	CoeffBound            *float64          `json:"coeff_bound"`
	EWMAAlpha             *float64          `json:"ewma_alpha"`
	VerbStatsColdStartN   *uint64           `json:"verbstats_cold_start_n"`
	OvershootThreshold    *float64          `json:"overshoot_threshold"`
	OvershootWindow       *int              `json:"overshoot_window"`
	OvershootMaxTrips     *int              `json:"overshoot_max_trips"`
	OvershootGrace        *int              `json:"overshoot_grace"`
	ResidualNormFloor     *float64          `json:"residual_norm_floor"`
	DefaultAvgTxRLP       *float64          `json:"default_avg_tx_rlp"`
	DefaultSigma          *float64          `json:"default_sigma"`
	AvgTxRLPDecayNew      *float64          `json:"avg_tx_rlp_decay_new"`
	DefaultBaseGasPerVerb *uint64           `json:"default_base_gas_per_verb"`
	BaseGasPerVerb        map[string]uint64 `json:"base_gas_per_verb"`
}

type costYAML struct {
	PriorityTipWei      *int64 `json:"priority_tip_wei"`
	FeePolicyIntervalMS *int64 `json:"fee_policy_interval_ms"`
	EthPerGasTarget     *int64 `json:"eth_per_gas_target"`
}

type runYAML struct {
	TotalBatchBytes        *int    `json:"total_batch_bytes"`
	TotalBatchBytesCap     *int    `json:"total_batch_bytes_cap"`
	AddressStride          *uint64 `json:"address_stride"`
	DispatchSkipStreakHalt *int    `json:"dispatch_skip_streak_halt"`
	RejectionStreakHalt    *int    `json:"rejection_streak_halt"`
	IdleBackoffMS          *int64  `json:"idle_backoff_ms"`
	ResumeReorgTolerance   *int64  `json:"resume_reorg_tolerance"`
	RPCTimeoutS            *int64  `json:"rpc_timeout_s"`
	SensorPollIntervalMS   *int64  `json:"sensor_poll_interval_ms"`
	SensorDeadlineMS       *int64  `json:"sensor_deadline_ms"`
	SensorPollGapMS        *int64  `json:"sensor_poll_gap_ms"`
	SensorLogIntervalS     *int64  `json:"sensor_log_interval_s"`
	MetricsAddr            *string `json:"metrics_addr"`
	TargetWatchDebounceMS  *int64  `json:"target_watch_debounce_ms"`
	ContractDeployGas      *uint64 `json:"contract_deploy_gas"`
}

// Load resolves a RunConfig for the run. Precedence, low to high:
//  1. Defaults() — the historical hardcoded constants.
//  2. The optional control: / cost: / run: blocks in target.yaml.
//  3. Environment-variable overrides for keys that historically had env vars.
//
// targetYAMLPath may be empty, in which case only steps 1 and 3 apply. Load
// then validates the result and returns a clear error on any out-of-range
// value so a misconfigured run fails fast at startup.
func Load(targetYAMLPath string) (RunConfig, error) {
	cfg := Defaults()

	if targetYAMLPath != "" {
		raw, err := os.ReadFile(targetYAMLPath)
		if err != nil {
			return RunConfig{}, fmt.Errorf("config: read %s: %w", targetYAMLPath, err)
		}
		var yf yamlFile
		if err := yaml.Unmarshal(raw, &yf); err != nil {
			return RunConfig{}, fmt.Errorf("config: parse %s: %w", targetYAMLPath, err)
		}
		applyYAML(&cfg, &yf)
	}

	if err := applyEnv(&cfg); err != nil {
		return RunConfig{}, err
	}

	if err := cfg.validate(); err != nil {
		return RunConfig{}, err
	}
	return cfg, nil
}

// applyYAML overlays any present target.yaml block onto cfg.
func applyYAML(cfg *RunConfig, yf *yamlFile) {
	if c := yf.Control; c != nil {
		setF(&cfg.Control.Epsilon, c.Epsilon)
		setF(&cfg.Control.ProjectionEta, c.ProjectionEta)
		setF(&cfg.Control.ToleranceFloor, c.ToleranceFloor)
		setF(&cfg.Control.GasFillFraction, c.GasFillFraction)
		setF(&cfg.Control.GasCapFraction, c.GasCapFraction)
		setI(&cfg.Control.NMaxHardCeil, c.NMaxHardCeil)
		setF(&cfg.Control.AlphaMin, c.AlphaMin)
		setF(&cfg.Control.AlphaMax, c.AlphaMax)
		setF(&cfg.Control.SigmoidCenter, c.SigmoidCenter)
		setF(&cfg.Control.SigmoidK, c.SigmoidK)
		setF(&cfg.Control.SigmaFloor, c.SigmaFloor)
		setF(&cfg.Control.AlphaEpsilon, c.AlphaEpsilon)
		setF(&cfg.Control.SigmaEWMADecay, c.SigmaEWMADecay)
		setF(&cfg.Control.AlphaEWMADecay, c.AlphaEWMADecay)
		setF(&cfg.Control.CoeffBound, c.CoeffBound)
		setF(&cfg.Control.EWMAAlpha, c.EWMAAlpha)
		setU64(&cfg.Control.VerbStatsColdStartN, c.VerbStatsColdStartN)
		setF(&cfg.Control.OvershootThreshold, c.OvershootThreshold)
		setI(&cfg.Control.OvershootWindow, c.OvershootWindow)
		setI(&cfg.Control.OvershootMaxTrips, c.OvershootMaxTrips)
		setI(&cfg.Control.OvershootGrace, c.OvershootGrace)
		setF(&cfg.Control.ResidualNormFloor, c.ResidualNormFloor)
		setF(&cfg.Control.DefaultAvgTxRLP, c.DefaultAvgTxRLP)
		setF(&cfg.Control.DefaultSigma, c.DefaultSigma)
		setF(&cfg.Control.AvgTxRLPDecayNew, c.AvgTxRLPDecayNew)
		setU64(&cfg.Control.DefaultBaseGasPerVerb, c.DefaultBaseGasPerVerb)
		if c.BaseGasPerVerb != nil {
			m := make(map[string]uint64, len(c.BaseGasPerVerb))
			for k, v := range c.BaseGasPerVerb {
				m[k] = v
			}
			cfg.Control.BaseGasPerVerb = m
		}
	}
	if c := yf.Cost; c != nil {
		setI64(&cfg.Cost.PriorityTipWei, c.PriorityTipWei)
		setI64(&cfg.Cost.FeePolicyIntervalMS, c.FeePolicyIntervalMS)
		setI64(&cfg.Cost.EthPerGasTarget, c.EthPerGasTarget)
	}
	if r := yf.Run; r != nil {
		setI(&cfg.Run.TotalBatchBytes, r.TotalBatchBytes)
		setI(&cfg.Run.TotalBatchBytesCap, r.TotalBatchBytesCap)
		setU64(&cfg.Run.AddressStride, r.AddressStride)
		setI(&cfg.Run.DispatchSkipStreakHalt, r.DispatchSkipStreakHalt)
		setI(&cfg.Run.RejectionStreakHalt, r.RejectionStreakHalt)
		setI64(&cfg.Run.IdleBackoffMS, r.IdleBackoffMS)
		setI64(&cfg.Run.ResumeReorgTolerance, r.ResumeReorgTolerance)
		setI64(&cfg.Run.RPCTimeoutS, r.RPCTimeoutS)
		setI64(&cfg.Run.SensorPollIntervalMS, r.SensorPollIntervalMS)
		setI64(&cfg.Run.SensorDeadlineMS, r.SensorDeadlineMS)
		setI64(&cfg.Run.SensorPollGapMS, r.SensorPollGapMS)
		setI64(&cfg.Run.SensorLogIntervalS, r.SensorLogIntervalS)
		setStr(&cfg.Run.MetricsAddr, r.MetricsAddr)
		setI64(&cfg.Run.TargetWatchDebounceMS, r.TargetWatchDebounceMS)
		setU64(&cfg.Run.ContractDeployGas, r.ContractDeployGas)
	}
}

func setF(dst *float64, v *float64) {
	if v != nil {
		*dst = *v
	}
}
func setI(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
func setI64(dst *int64, v *int64) {
	if v != nil {
		*dst = *v
	}
}
func setU64(dst *uint64, v *uint64) {
	if v != nil {
		*dst = *v
	}
}
func setStr(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

// applyEnv overlays the historical environment-variable overrides. Every env
// var the orchestrator previously honoured is still honoured here so existing
// deployment scripts keep working. A malformed value is a hard error — a typo
// in an env var must fail fast, not silently fall back.
func applyEnv(cfg *RunConfig) error {
	if raw, ok := os.LookupEnv("ORCH_EPSILON"); ok && raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("config: ORCH_EPSILON=%q: %w", raw, err)
		}
		cfg.Control.Epsilon = v
	}
	if raw, ok := os.LookupEnv("ORCH_EWMA_ALPHA"); ok && raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("config: ORCH_EWMA_ALPHA=%q: %w", raw, err)
		}
		cfg.Control.EWMAAlpha = v
	}
	if raw, ok := os.LookupEnv("ORCH_OVERSHOOT_THRESHOLD"); ok && raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("config: ORCH_OVERSHOOT_THRESHOLD=%q: %w", raw, err)
		}
		cfg.Control.OvershootThreshold = v
	}
	if raw, ok := os.LookupEnv("ORCH_TOTAL_BATCH_BYTES"); ok && raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("config: ORCH_TOTAL_BATCH_BYTES=%q: %w", raw, err)
		}
		cfg.Run.TotalBatchBytes = v
	}
	if raw, ok := os.LookupEnv("ORCH_RPC_TIMEOUT_S"); ok && raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("config: ORCH_RPC_TIMEOUT_S=%q: %w", raw, err)
		}
		cfg.Run.RPCTimeoutS = v
	}
	if raw, ok := os.LookupEnv("ORCH_RESUME_REORG_TOLERANCE"); ok && raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("config: ORCH_RESUME_REORG_TOLERANCE=%q: %w", raw, err)
		}
		cfg.Run.ResumeReorgTolerance = v
	}
	return nil
}

// EnvTruthy reports whether an environment variable is set to a truthy value.
// Accepted truthy values: "1", "true", "yes", "on" (case-insensitive). It is
// the single home for the orchestrator's env-flag parsing (ORCH_DEBUG_PICK,
// ORCH_ALLOW_NON_ZERO_FRESH_HEAD).
func EnvTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Validate range-checks a RunConfig. It is exported so callers that mutate a
// loaded config (e.g. CLI-flag overrides) can re-check it before use. Load
// always calls it; a bad value fails fast with a clear error.
func Validate(c RunConfig) error { return c.validate() }

// validate range-checks the resolved config. It fails fast with a clear error
// so a bad target.yaml value or env override is caught at startup.
func (c RunConfig) validate() error {
	ck := c.Control
	if !inUnit(ck.Epsilon) {
		return rangeErr("control.epsilon", ck.Epsilon, "[0, 1]")
	}
	if ck.ProjectionEta <= 0 {
		return rangeErr("control.projection_eta", ck.ProjectionEta, "> 0")
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
	if r.TotalBatchBytes < 1024 {
		return rangeErr("run.total_batch_bytes", float64(r.TotalBatchBytes), ">= 1024")
	}
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
	return nil
}

func inUnit(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}

func rangeErr(key string, v float64, want string) error {
	return fmt.Errorf("config: %s=%g out of range, want %s", key, v, want)
}
