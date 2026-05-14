// Package mode owns the orchestrator's run-mode state machine. ModeController
// is shared between healthd (which requests transitions on staleness signals)
// and lifecycle (which observes the current mode to gate dispatch). Living in
// its own package keeps both importers free of circular dependencies.
package mode

import (
	"log/slog"
	"sync"
	"sync/atomic"
)

// Mode is the orchestrator's run-state. Transitions are one-way except
// Throttle ↔ Run, and Halt is terminal.
type Mode int32

const (
	ModeRun Mode = iota
	ModeThrottle
	ModeDrain
	ModeHalt
)

// String returns a stable label for logs and metric labels.
func (m Mode) String() string {
	switch m {
	case ModeRun:
		return "run"
	case ModeThrottle:
		return "throttle"
	case ModeDrain:
		return "drain"
	case ModeHalt:
		return "halt"
	default:
		return "unknown"
	}
}

// ModeController serialises transitions through mu while exposing lock-free
// reads via atomic load on the mode int32. Callers that only need the current
// mode call Get; callers that change it use the Request* methods, which are
// idempotent and ignore invalid transitions.
type ModeController struct {
	mode atomic.Int32
	mu   sync.Mutex
}

// NewModeController returns a controller in ModeRun.
func NewModeController() *ModeController {
	c := &ModeController{}
	c.mode.Store(int32(ModeRun))
	return c
}

// Get returns the current mode via an atomic load.
func (c *ModeController) Get() Mode {
	return Mode(c.mode.Load())
}

// RequestRun lifts Throttle back to Run when the degradation clears.
// Ignored from any other state.
func (c *ModeController) RequestRun(reason string) {
	c.transition(ModeRun, reason, func(from Mode) bool {
		return from == ModeThrottle
	})
}

// RequestThrottle moves Run → Throttle. Ignored if already throttled or in a
// terminal state.
func (c *ModeController) RequestThrottle(reason string) {
	c.transition(ModeThrottle, reason, func(from Mode) bool {
		return from == ModeRun
	})
}

// RequestDrain moves Run/Throttle → Drain. Ignored from Drain or Halt.
func (c *ModeController) RequestDrain(reason string) {
	c.transition(ModeDrain, reason, func(from Mode) bool {
		return from == ModeRun || from == ModeThrottle
	})
}

// RequestHalt moves any non-Halt state → Halt. Always wins.
func (c *ModeController) RequestHalt(reason string) {
	c.transition(ModeHalt, reason, func(from Mode) bool {
		return from != ModeHalt
	})
}

func (c *ModeController) transition(to Mode, reason string, allowed func(from Mode) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := Mode(c.mode.Load())
	if from == to {
		return
	}
	if !allowed(from) {
		return
	}
	c.mode.Store(int32(to))
	slog.Warn("mode transition", "from", from.String(), "to", to.String(), "reason", reason)
	// TODO(metrics): wire orch_mode_transitions_total
}
