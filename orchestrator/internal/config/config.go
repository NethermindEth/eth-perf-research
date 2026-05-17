// Package config defines RunConfig, the single source of truth for every
// tunable orchestrator value. Before this package each subsystem carried its
// own package-level magic constants and read environment variables ad hoc; a
// co-design review flagged ~40 such scattered constants across six packages.
//
// RunConfig collapses all of them into one struct with three groups —
// Control (controller tuning), Cost (fee policy), and Run (run-loop / sensor /
// bootstrap) — populated by Load from built-in defaults, optional target.yaml
// blocks, and environment-variable overrides, in that precedence order.
//
// Defaults() returns a RunConfig whose every field equals the historical
// hardcoded constant, so resolving the config changes no behaviour.
package config

// Control groups the controller-tuning parameters.
type Control struct {
	// Epsilon is the ε-greedy verb-exploration rate. Historical: defaultEpsilon.
	Epsilon float64 `json:"epsilon"`
	// ProjectionEta is the projected-gradient step size in Pick.
	ProjectionEta float64 `json:"projection_eta"`
	// ToleranceFloor is the minimum per-axis tolerance fraction. It also
	// defines the gradient-equivalence band for the BytesPerGas tie-breaker:
	// verbs whose gradient scores are within ToleranceFloor of the best are
	// treated as equivalent.
	ToleranceFloor float64 `json:"tolerance_floor"`
	// EntropyFloor (R1) is the minimum simplex weight every eligible verb (one
	// with a non-degenerate F-row) keeps after the Michelot projection, so a
	// verb that is the sole grower of an under-target axis can never be zeroed
	// out of selection.
	EntropyFloor float64 `json:"entropy_floor"`
	// AntiWindupEnabled (R2) gates the per-axis anti-windup clamp: when an axis
	// is at or above target, its away-pushing gradient contribution is clamped
	// to zero so the controller stops integrating error on a satisfied axis.
	AntiWindupEnabled bool `json:"anti_windup_enabled"`
	// BytesPerGasTieBreak gates the BytesPerGas tie-breaker among
	// gradient-equivalent verbs (Change 4 / design-v3 §B).
	BytesPerGasTieBreak bool `json:"bytes_per_gas_tie_break"`
	// GasFillFraction is the fraction of block gas Pick targets when sizing a batch.
	GasFillFraction float64 `json:"gas_fill_fraction"`
	// GasCapFraction is the dispatcher's hard gas ceiling fraction.
	GasCapFraction float64 `json:"gas_cap_fraction"`
	// NMaxHardCeil caps batch size for commit efficiency.
	NMaxHardCeil int `json:"nmax_hard_ceil"`

	// AlphaMin/AlphaMax bound the adaptive learning rate.
	AlphaMin float64 `json:"alpha_min"`
	AlphaMax float64 `json:"alpha_max"`
	// SigmoidCenter/SigmoidK shape the adaptive-α tanh response.
	SigmoidCenter float64 `json:"sigmoid_center"`
	SigmoidK      float64 `json:"sigmoid_k"`
	// SigmaFloor is the lower clamp on the σ innovation tracker.
	SigmaFloor float64 `json:"sigma_floor"`
	// AlphaEpsilon is the denominator floor in the absRatio computation.
	AlphaEpsilon float64 `json:"alpha_epsilon"`
	// SigmaEWMADecay/AlphaEWMADecay are the EWMA decay rates for σ and α.
	SigmaEWMADecay float64 `json:"sigma_ewma_decay"`
	AlphaEWMADecay float64 `json:"alpha_ewma_decay"`

	// CoeffBound is the physical magnitude ceiling for an F-coefficient / σ.
	CoeffBound float64 `json:"coeff_bound"`
	// EWMAAlpha is the per-verb stats EWMA decay (env ORCH_EWMA_ALPHA).
	EWMAAlpha float64 `json:"ewma_alpha"`
	// VerbStatsColdStartN is the sample count below which Pick uses baselines.
	VerbStatsColdStartN uint64 `json:"verbstats_cold_start_n"`

	// OvershootThreshold is the residual-ratio trip threshold (env
	// ORCH_OVERSHOOT_THRESHOLD).
	OvershootThreshold float64 `json:"overshoot_threshold"`
	// OvershootWindow / OvershootMaxTrips / OvershootGrace tune the rolling
	// instability detector.
	OvershootWindow   int `json:"overshoot_window"`
	OvershootMaxTrips int `json:"overshoot_max_trips"`
	OvershootGrace    int `json:"overshoot_grace"`

	// ResidualNormFloor is the denominator floor for the overshoot ratio.
	ResidualNormFloor float64 `json:"residual_norm_floor"`
	// DefaultAvgTxRLP / DefaultSigma seed AvgTxRLP and the σ matrix.
	DefaultAvgTxRLP float64 `json:"default_avg_tx_rlp"`
	DefaultSigma    float64 `json:"default_sigma"`
	// AvgTxRLPDecayNew is the weight given to the newest observation in the
	// AvgTxRLP EWMA; the prior keeps (1 - AvgTxRLPDecayNew).
	AvgTxRLPDecayNew float64 `json:"avg_tx_rlp_decay_new"`
	// DefaultBaseGasPerVerb is the per-tx gas estimate for an unknown verb.
	DefaultBaseGasPerVerb uint64 `json:"default_base_gas_per_verb"`
	// BaseGasPerVerb is the cold-start per-tx gas table (verb -> gas).
	BaseGasPerVerb map[string]uint64 `json:"base_gas_per_verb"`
}

// Cost groups the fee-policy parameters.
type Cost struct {
	// PriorityTipWei is the fixed EIP-1559 priority tip for signed txs.
	PriorityTipWei int64 `json:"priority_tip_wei"`
	// FeePolicyIntervalMS is the fee-policy refresh interval, in milliseconds.
	FeePolicyIntervalMS int64 `json:"fee_policy_interval_ms"`
	// EthPerGasTarget is reserved for a later step; nothing reads it yet.
	EthPerGasTarget int64 `json:"eth_per_gas_target"`
}

// Run groups the run-loop, sensor, and bootstrap parameters.
type Run struct {
	// TotalBatchBytes is the per-block tx-payload byte budget (env
	// ORCH_TOTAL_BATCH_BYTES). TotalBatchBytesCap is the hard upper clamp.
	TotalBatchBytes    int `json:"total_batch_bytes"`
	TotalBatchBytesCap int `json:"total_batch_bytes_cap"`
	// AddressStride is the per-revision address-domain stride.
	AddressStride uint64 `json:"address_stride"`

	// DispatchSkipStreakHalt / RejectionStreakHalt bound consecutive
	// dispatch/commit failures before the run halts. IdleBackoffMS is the
	// planner idle sleep, in milliseconds.
	DispatchSkipStreakHalt int   `json:"dispatch_skip_streak_halt"`
	RejectionStreakHalt    int   `json:"rejection_streak_halt"`
	IdleBackoffMS          int64 `json:"idle_backoff_ms"`

	// ResumeReorgTolerance bounds the head/journal-tail gap a resume tolerates
	// (env ORCH_RESUME_REORG_TOLERANCE).
	ResumeReorgTolerance int64 `json:"resume_reorg_tolerance"`
	// RPCTimeoutS is the JSON-RPC client timeout, in seconds (env
	// ORCH_RPC_TIMEOUT_S).
	RPCTimeoutS int64 `json:"rpc_timeout_s"`

	// Sensor poll/deadline/gap/log intervals.
	SensorPollIntervalMS int64 `json:"sensor_poll_interval_ms"`
	SensorDeadlineMS     int64 `json:"sensor_deadline_ms"`
	SensorPollGapMS      int64 `json:"sensor_poll_gap_ms"`
	SensorLogIntervalS   int64 `json:"sensor_log_interval_s"`

	// MetricsAddr is the Prometheus /metrics TCP address.
	MetricsAddr string `json:"metrics_addr"`
	// TargetWatchDebounceMS is the target.yaml fsnotify debounce, in ms.
	TargetWatchDebounceMS int64 `json:"target_watch_debounce_ms"`
	// ContractDeployGas is the gas limit for each bootstrap CREATE tx.
	ContractDeployGas uint64 `json:"contract_deploy_gas"`
}

// RunConfig is the single resolved configuration for an orchestrator run.
type RunConfig struct {
	Control Control `json:"control"`
	Cost    Cost    `json:"cost"`
	Run     Run     `json:"run"`
}

// defaultBaseGasPerVerb is the cold-start per-tx gas table. It is the built-in
// default for RunConfig.Control.BaseGasPerVerb; a target.yaml control block may
// override it wholesale.
func defaultBaseGasPerVerb() map[string]uint64 {
	return map[string]uint64{
		"eoatx":           21_000,
		"deploytx":        1_000_000,
		"factorydeploytx": 500_000,
		"storagespam":     2_050_000,
		"storagerefundtx": 3_000_000,
		"erc20tx":         100_000,
		"erc20_bloater":   16_700_000,
		"uniswap_swaps":   200_000,
		"gasburnertx":     2_000_000,
		"calltx":          500_000,
		"evm_fuzz":        1_000_000,
		"noop":            21_000,
	}
}

// Defaults returns a RunConfig whose every field equals the orchestrator's
// historical hardcoded constant. Resolving the config from Defaults() alone is
// behaviour-neutral.
func Defaults() RunConfig {
	return RunConfig{
		Control: Control{
			Epsilon:               0.5,
			ProjectionEta:         0.1,
			ToleranceFloor:        0.005,
			EntropyFloor:          0.02,
			AntiWindupEnabled:     true,
			BytesPerGasTieBreak:   true,
			GasFillFraction:       0.90,
			GasCapFraction:        0.95,
			NMaxHardCeil:          12000,
			AlphaMin:              0.02,
			AlphaMax:              0.30,
			SigmoidCenter:         0.08,
			SigmoidK:              25.0,
			SigmaFloor:            1.0,
			AlphaEpsilon:          1000.0,
			SigmaEWMADecay:        0.9,
			AlphaEWMADecay:        0.7,
			CoeffBound:            1e6,
			EWMAAlpha:             0.10,
			VerbStatsColdStartN:   10,
			OvershootThreshold:    0.20,
			OvershootWindow:       5,
			OvershootMaxTrips:     3,
			OvershootGrace:        5,
			ResidualNormFloor:     1024.0,
			DefaultAvgTxRLP:       1500.0,
			DefaultSigma:          1.0,
			AvgTxRLPDecayNew:      0.3,
			DefaultBaseGasPerVerb: 1_000_000,
			BaseGasPerVerb:        defaultBaseGasPerVerb(),
		},
		Cost: Cost{
			PriorityTipWei:      1_000_000_000,
			FeePolicyIntervalMS: 1000,
			EthPerGasTarget:     1_000_000_000,
		},
		Run: Run{
			TotalBatchBytes:        2 * 1024 * 1024,
			TotalBatchBytesCap:     7680 * 1024,
			AddressStride:          uint64(1) << 40,
			DispatchSkipStreakHalt: 20,
			RejectionStreakHalt:    5,
			IdleBackoffMS:          50,
			ResumeReorgTolerance:   256,
			RPCTimeoutS:            120,
			SensorPollIntervalMS:   100,
			SensorDeadlineMS:       2000,
			SensorPollGapMS:        2000,
			SensorLogIntervalS:     30,
			MetricsAddr:            ":9101",
			TargetWatchDebounceMS:  100,
			ContractDeployGas:      2_000_000,
		},
	}
}
