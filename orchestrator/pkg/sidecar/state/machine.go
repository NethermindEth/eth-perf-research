// Package state defines the sidecar's lifecycle state machine.
//
//	STARTING → (snapshot? ) → CATCHING_UP → LIVE_TAILING
//	         \                ↗
//	          → BOOTSTRAP_SCAN
//
// Transitions are advanced by the orchestrator in cmd/sidecar; this package
// only carries the enum + a thread-safe holder so anything (metrics, RPC,
// shutdown) can observe progress without coordinating directly with the
// orchestrator.
package state

import (
	"sync/atomic"
	"time"
)

// Phase enumerates the lifecycle phases.
type Phase int32

const (
	PhaseStarting Phase = iota
	PhaseBootstrap
	PhaseCatchingUp
	PhaseLive
	PhaseShutdown
)

// String returns the phase name (for logs / metrics).
func (p Phase) String() string {
	switch p {
	case PhaseStarting:
		return "STARTING"
	case PhaseBootstrap:
		return "BOOTSTRAP_SCAN"
	case PhaseCatchingUp:
		return "CATCHING_UP"
	case PhaseLive:
		return "LIVE_TAILING"
	case PhaseShutdown:
		return "SHUTDOWN"
	default:
		return "UNKNOWN"
	}
}

// Machine is the thread-safe phase holder.
type Machine struct {
	phase     atomic.Int32
	enteredAt atomic.Int64 // unix-ns
}

// New constructs a fresh machine in PhaseStarting.
func New() *Machine {
	m := &Machine{}
	m.phase.Store(int32(PhaseStarting))
	m.enteredAt.Store(time.Now().UnixNano())
	return m
}

// Phase returns the current phase.
func (m *Machine) Phase() Phase {
	return Phase(m.phase.Load())
}

// EnteredAt returns the wall-clock time at which the current phase was entered.
func (m *Machine) EnteredAt() time.Time {
	return time.Unix(0, m.enteredAt.Load())
}

// Advance transitions to the next phase. Returns false if the requested
// transition is not legal (e.g. going from LIVE_TAILING back to BOOTSTRAP).
func (m *Machine) Advance(to Phase) bool {
	for {
		from := Phase(m.phase.Load())
		if !validTransition(from, to) {
			return false
		}
		if m.phase.CompareAndSwap(int32(from), int32(to)) {
			m.enteredAt.Store(time.Now().UnixNano())
			return true
		}
	}
}

func validTransition(from, to Phase) bool {
	if to == PhaseShutdown {
		return true // shutdown is always permitted
	}
	switch from {
	case PhaseStarting:
		return to == PhaseBootstrap || to == PhaseCatchingUp
	case PhaseBootstrap:
		return to == PhaseCatchingUp
	case PhaseCatchingUp:
		return to == PhaseLive
	}
	return false
}
