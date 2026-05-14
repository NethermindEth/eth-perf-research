// Package healthd watches the chain-head / sensor-head delta and asks the mode
// controller to throttle or halt when the sensor falls too far behind the
// chain. Recovery requires N consecutive OK ticks so that a single fast poll
// doesn't ping-pong the orchestrator between Throttle and Run.
package healthd

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mode"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

// StalenessLevel grades how far the sensor is behind the chain.
type StalenessLevel int

const (
	StalenessOK StalenessLevel = iota
	StalenessDegraded
	StalenessCritical
)

func (l StalenessLevel) String() string {
	switch l {
	case StalenessOK:
		return "ok"
	case StalenessDegraded:
		return "degraded"
	case StalenessCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Staleness is the most recent snapshot Health computed. Safe to read after
// Run returns; while Run is active, prefer Snapshot which returns a copy.
type Staleness struct {
	BlocksBehind  uint64
	SecondsBehind float64
	Level         StalenessLevel
}

// Thresholds controls when staleness escalates. All values may be overridden
// via env vars; see DefaultThresholds for the names. Zero blocks/seconds means
// "this check is disabled".
type Thresholds struct {
	ThrottleBlocks  uint64
	HaltBlocks      uint64
	ThrottleSeconds float64
	HaltSeconds     float64
	RecoveryTicks   int
	TickInterval    time.Duration
}

const (
	defaultThrottleBlocks  uint64        = 200
	defaultHaltBlocks      uint64        = 2000
	defaultThrottleSeconds float64       = 30
	defaultHaltSeconds     float64       = 300
	defaultRecoveryTicks                 = 3
	defaultTickInterval    time.Duration = 500 * time.Millisecond
)

// DefaultThresholds returns the production defaults, with env-var overrides
// applied where present. Invalid values fall back to defaults silently rather
// than failing startup — healthd must never block the orchestrator.
func DefaultThresholds() Thresholds {
	return Thresholds{
		ThrottleBlocks:  envUint("ORCH_HEALTH_THROTTLE_BLOCKS", defaultThrottleBlocks),
		HaltBlocks:      envUint("ORCH_HEALTH_HALT_BLOCKS", defaultHaltBlocks),
		ThrottleSeconds: envFloat("ORCH_HEALTH_THROTTLE_SECONDS", defaultThrottleSeconds),
		HaltSeconds:     envFloat("ORCH_HEALTH_HALT_SECONDS", defaultHaltSeconds),
		RecoveryTicks:   defaultRecoveryTicks,
		TickInterval:    defaultTickInterval,
	}
}

// HeadFetcher is the RPC surface healthd needs.
type HeadFetcher interface {
	BlockByNumber(ctx context.Context, n int64) (*rpc.BlockHeader, error)
}

// SensorPoller is the sensor surface healthd needs.
type SensorPoller interface {
	PollOnce(ctx context.Context) (*sensor.Snapshot, error)
}

// Health is the runtime monitor. Construct via New, then call Run in a
// goroutine; Snapshot is safe to call from any goroutine while Run is active.
type Health struct {
	rpc        HeadFetcher
	sensor     SensorPoller
	mode       *mode.ModeController
	thresholds Thresholds

	headBN   atomic.Uint64
	sensorBN atomic.Uint64

	// staleness is the last computed view; readers Get a copy via Snapshot.
	stalenessMu     atomic.Pointer[Staleness]
	lastAdvanceUnix atomic.Int64
	okStreak        int
}

// New constructs a Health monitor with default thresholds.
func New(r HeadFetcher, s SensorPoller, m *mode.ModeController) *Health {
	return NewWithThresholds(r, s, m, DefaultThresholds())
}

// NewWithThresholds constructs a Health monitor with explicit thresholds.
func NewWithThresholds(r HeadFetcher, s SensorPoller, m *mode.ModeController, th Thresholds) *Health {
	if th.TickInterval <= 0 {
		th.TickInterval = defaultTickInterval
	}
	if th.RecoveryTicks <= 0 {
		th.RecoveryTicks = defaultRecoveryTicks
	}
	h := &Health{
		rpc:        r,
		sensor:     s,
		mode:       m,
		thresholds: th,
	}
	h.lastAdvanceUnix.Store(time.Now().Unix())
	h.stalenessMu.Store(&Staleness{Level: StalenessOK})
	return h
}

// Run ticks at TickInterval until ctx is cancelled.
func (h *Health) Run(ctx context.Context) error {
	tick := time.NewTicker(h.thresholds.TickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			h.step(ctx, now)
		}
	}
}

// Snapshot returns the most recent staleness reading.
func (h *Health) Snapshot() Staleness {
	if s := h.stalenessMu.Load(); s != nil {
		return *s
	}
	return Staleness{Level: StalenessOK}
}

func (h *Health) step(ctx context.Context, now time.Time) {
	chainHead, err := h.rpc.BlockByNumber(ctx, -1)
	if err != nil {
		if ctx.Err() == nil {
			slog.Debug("healthd: chain head fetch failed", "err", err)
		}
		return
	}
	h.headBN.Store(chainHead.Number)

	snap, err := h.sensor.PollOnce(ctx)
	if err != nil || snap == nil {
		if err != nil && ctx.Err() == nil {
			slog.Debug("healthd: sensor poll failed", "err", err)
		}
		return
	}

	prevSensor := h.sensorBN.Swap(snap.BlockNumber)
	if snap.BlockNumber > prevSensor {
		h.lastAdvanceUnix.Store(now.Unix())
	}

	var blocksBehind uint64
	if chainHead.Number > snap.BlockNumber {
		blocksBehind = chainHead.Number - snap.BlockNumber
	}
	secondsBehind := float64(now.Unix() - h.lastAdvanceUnix.Load())
	if secondsBehind < 0 {
		secondsBehind = 0
	}

	level := classify(blocksBehind, secondsBehind, h.thresholds)
	h.stalenessMu.Store(&Staleness{
		BlocksBehind:  blocksBehind,
		SecondsBehind: secondsBehind,
		Level:         level,
	})

	h.react(level, blocksBehind, secondsBehind)
}

func classify(blocksBehind uint64, secondsBehind float64, t Thresholds) StalenessLevel {
	level := StalenessOK
	if t.ThrottleBlocks > 0 && blocksBehind > t.ThrottleBlocks {
		level = StalenessDegraded
	}
	if t.ThrottleSeconds > 0 && secondsBehind > t.ThrottleSeconds {
		level = StalenessDegraded
	}
	if t.HaltBlocks > 0 && blocksBehind > t.HaltBlocks {
		level = StalenessCritical
	}
	if t.HaltSeconds > 0 && secondsBehind > t.HaltSeconds {
		level = StalenessCritical
	}
	return level
}

// react translates the level into a mode request. Halt is unconditional;
// throttle fires on the first degraded tick; run-recovery only fires after
// RecoveryTicks consecutive OK readings so a single quick poll can't bounce
// the orchestrator between throttle and run.
func (h *Health) react(level StalenessLevel, blocksBehind uint64, secondsBehind float64) {
	switch level {
	case StalenessCritical:
		h.okStreak = 0
		h.mode.RequestHalt(formatReason("sensor critical", blocksBehind, secondsBehind))
	case StalenessDegraded:
		h.okStreak = 0
		h.mode.RequestThrottle(formatReason("sensor degraded", blocksBehind, secondsBehind))
	case StalenessOK:
		h.okStreak++
		if h.okStreak >= h.thresholds.RecoveryTicks {
			h.mode.RequestRun(formatReason("sensor recovered", blocksBehind, secondsBehind))
		}
	}
}

func formatReason(prefix string, blocksBehind uint64, secondsBehind float64) string {
	return prefix + " (" +
		"blocks=" + strconv.FormatUint(blocksBehind, 10) +
		" seconds=" + strconv.FormatFloat(secondsBehind, 'f', 1, 64) +
		")"
}

func envUint(key string, def uint64) uint64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}
